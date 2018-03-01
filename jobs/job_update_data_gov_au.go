package jobs

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"time"

	que "github.com/bgentry/que-go"
	ct "github.com/google/certificate-transparency-go"
	cttls "github.com/google/certificate-transparency-go/tls"
	ctx509 "github.com/google/certificate-transparency-go/x509"
	"github.com/jackc/pgx"
)

const (
	KeyUpdateDataGovAU   = "update_data_gov_au"
	KeyBackfillDataGovAU = "backfill_data_gov_au"
	MaxToBackfill        = 256
)

func (us *UpdateDataGovAU) BackfillDataGovAU(qc *que.Client, logger *log.Logger, job *que.Job, tx *pgx.Tx) error {
	processed := 0
	rows, err := tx.Query("SELECT key, leaf FROM cert_store WHERE needs_ckan_backfill = TRUE LIMIT $1", MaxToBackfill)
	if err != nil {
		return err
	}
	defer rows.Close()

	var keys [][]byte
	var ckanRecs []*ckanRecord
	for rows.Next() {
		var key, leafData []byte
		err = rows.Scan(&key, &leafData)
		if err != nil {
			return err
		}

		keys = append(keys, key)
		r, err := makeGovAURecord(leafData)
		if err != nil {
			return err
		}
		ckanRecs = append(ckanRecs, r)

		processed++
	}
	rows.Close()

	for _, k := range keys {
		_, err = tx.Exec("UPDATE cert_store SET needs_ckan_backfill = $1 WHERE key = $2", false, k)
		if err != nil {
			return err
		}
	}

	err = us.InsertRecords(logger, ckanRecs)
	if err != nil {
		return err
	}

	logger.Printf("Updated %d records", processed)

	// If we got any, this will commit and try again
	if processed > 0 {
		return ErrImmediateReschedule
	}

	// Returning nil will commit and reschedule via cron
	return nil
}

func makeGovAURecord(b []byte) (*ckanRecord, error) {
	var leaf ct.MerkleTreeLeaf
	_, err := cttls.Unmarshal(b, &leaf)
	if err != nil {
		return nil, err
	}

	var cert *ctx509.Certificate
	switch leaf.TimestampedEntry.EntryType {
	case ct.X509LogEntryType:
		cert, _ = leaf.X509Certificate()
	case ct.PrecertLogEntryType:
		cert, _ = leaf.Precertificate()
	}

	var nvb, nva time.Time
	var issuer string
	var domains []string
	if cert != nil {
		nva = cert.NotAfter
		nvb = cert.NotBefore
		issuer = cert.Issuer.CommonName

		doms := make(map[string]bool)
		if strings.HasSuffix(cert.Subject.CommonName, DomainSuffix) || cert.Subject.CommonName == MatchDomain {
			doms[cert.Subject.CommonName] = true
		}
		for _, name := range cert.DNSNames {
			if strings.HasSuffix(name, DomainSuffix) || name == MatchDomain {
				doms[name] = true
			}
		}
		for k := range doms {
			domains = append(domains, k)
		}
	}
	kh := sha256.Sum256(b)

	return &ckanRecord{
		"key":              kh[:],
		"issuer_cn":        issuer,
		"domains":          domains,
		"not_valid_before": nvb,
		"not_valid_after":  nva,
		"raw_data":         b,
	}, nil
}

type UpdateDataGovAUConf struct {
	Data []byte
}

type UpdateDataGovAU struct {
	BaseURL    string
	APIKey     string
	ResourceID string
}

type ckanField struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type ckanRecord map[string]interface{}

func (us *UpdateDataGovAU) InsertRecords(logger *log.Logger, recs []*ckanRecord) error {
	payload, err := json.Marshal(&struct {
		ResourceID string `json:"resource_id"`
		//Fields     []ckanField   `json:"fields"`
		//PrimaryKey string        `json:"primary_key"`
		Records []*ckanRecord `json:"records"`
		Method  string        `json:"method"`
	}{
		ResourceID: us.ResourceID,
		// Fields: []ckanField{
		// 	{ID: "key", Type: "text"},
		// 	{ID: "issuer_cn", Type: "text"},
		// 	{ID: "domains", Type: "text[]"},
		// 	{ID: "not_valid_before", Type: "timestamp"},
		// 	{ID: "not_valid_after", Type: "timestamp"},
		// 	{ID: "raw_data", Type: "text"},
		// },
		// PrimaryKey: "key",
		Records: recs,
		Method:  "upsert",
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, us.BaseURL+"/api/3/action/datastore_upsert", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", us.APIKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	returnVal, _ := ioutil.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		logger.Printf("Error value returned:\n%s\n", returnVal)
		return fmt.Errorf("bad status from data.gov.au: %v (%s)", resp.StatusCode, resp.Status)
	}

	return nil
}

func (us *UpdateDataGovAU) Run(qc *que.Client, logger *log.Logger, job *que.Job, tx *pgx.Tx) error {
	// If we don't use data.gov.au, fail fast
	if us.APIKey == "" {
		return nil
	}

	var conf UpdateDataGovAUConf
	err := json.Unmarshal(job.Args, &conf)
	if err != nil {
		return err
	}

	rec, err := makeGovAURecord(conf.Data)
	if err != nil {
		return err
	}

	return us.InsertRecords(logger, []*ckanRecord{rec})
}
