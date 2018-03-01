package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	que "github.com/bgentry/que-go"
	ct "github.com/google/certificate-transparency-go"
	ctclient "github.com/google/certificate-transparency-go/client"
	ctjsonclient "github.com/google/certificate-transparency-go/jsonclient"
	cttls "github.com/google/certificate-transparency-go/tls"
	ctx509 "github.com/google/certificate-transparency-go/x509"
	"github.com/jackc/pgx"
)

type GetEntriesConf struct {
	URL        string
	Start, End uint64 // end is exclusive
}

const (
	KeyGetEntries     = "get_entries"
	KeyUpdateMetadata = "update_metadata"

	DomainSuffix = ".gov.au"
	MatchDomain  = "gov.au"

	MaxToRequest = 1024

	MaxToUpdate = 1024
)

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

var (
	Jurisdictions = map[string]string{
		"TAS": "tas.gov.au",
		"VIC": "vic.gov.au",
		"NSW": "nsw.gov.au",
		"QLD": "qld.gov.au",
		"WA":  "wa.gov.au",
		"SA":  "sa.gov.au",
		"NT":  "nt.gov.au",
		"ACT": "act.gov.au",
	}

	CDNs = map[string]string{
		"cloudflaressl": "CloudFlare",
		"incapsula":     "Incapsula",
		"fastly":        "Fastly",
		"pantheonsite":  "PantheonSite",
	}
)

// Extract metadata for cert
func getFieldsAndValsForCert(leaf *ct.MerkleTreeLeaf) map[string]interface{} {
	var cert *ctx509.Certificate
	switch leaf.TimestampedEntry.EntryType {
	case ct.X509LogEntryType:
		cert, _ = leaf.X509Certificate()
	case ct.PrecertLogEntryType:
		cert, _ = leaf.Precertificate()
	}

	var nvb, nva time.Time
	var issuer string
	if cert != nil {
		nva = cert.NotAfter
		nvb = cert.NotBefore
		issuer = cert.Issuer.CommonName
	}

	var jurisdiction string
	for _, dom := range cert.DNSNames {
		for k, j := range Jurisdictions {
			if strings.HasSuffix(dom, "."+j) || dom == j {
				switch jurisdiction {
				case "":
					jurisdiction = k
				case k:
					continue
				default:
					jurisdiction = "MIXED"
				}
			}
		}
	}
	if jurisdiction == "" {
		jurisdiction = "OTHER"
	}

	var cdn string
	for k, c := range CDNs {
		if strings.Index(cert.Subject.CommonName, k) >= 0 {
			if cdn == "" {
				cdn = c
			} else {
				cdn = "MIXED"
			}
		}
	}
	if cdn == "" {
		cdn = "NOT RECOGNIZED CDN"
	}

	return map[string]interface{}{
		"not_valid_after":  nva,
		"not_valid_before": nvb,
		"issuer_cn":        issuer,
		"jurisdiction":     jurisdiction,
		"cdn":              cdn,
		"needs_update":     false,
	}
}

func RefreshMetadataForEntries(qc *que.Client, logger *log.Logger, job *que.Job, tx *pgx.Tx) error {
	processed := 0
	rows, err := tx.Query("SELECT key, leaf FROM cert_store WHERE needs_update = TRUE LIMIT $1", MaxToUpdate)
	if err != nil {
		return err
	}
	defer rows.Close()

	var updates []string
	var valvals [][]interface{}
	for rows.Next() {
		var key, leafData []byte
		err = rows.Scan(&key, &leafData)
		if err != nil {
			return err
		}

		var leaf ct.MerkleTreeLeaf
		_, err := cttls.Unmarshal(leafData, &leaf)
		if err != nil {
			return err
		}

		var sets []string
		var vals []interface{}
		cnt := 1
		for k, v := range getFieldsAndValsForCert(&leaf) {
			sets = append(sets, fmt.Sprintf("%s = $%d", k, cnt))
			vals = append(vals, v)
			cnt++
		}
		vals = append(vals, key)

		updates = append(updates, fmt.Sprintf("UPDATE cert_store SET %s WHERE key = $%d", strings.Join(sets, ", "), cnt))
		valvals = append(valvals, vals)

		processed++
	}
	rows.Close()

	for i := 0; i < processed; i++ {
		_, err = tx.Exec(updates[i], valvals[i]...)
		if err != nil {
			return err
		}
	}

	logger.Printf("Updated %d records", processed)

	// If we got any, try again
	if processed > 0 {
		return ErrImmediateReschedule
	}

	// Returning nil will commit and reschedule via cron
	return nil
}

func GetEntries(qc *que.Client, logger *log.Logger, job *que.Job, tx *pgx.Tx) error {
	var md GetEntriesConf
	err := json.Unmarshal(job.Args, &md)
	if err != nil {
		return err
	}

	lc, err := ctclient.New(fmt.Sprintf("https://%s", md.URL), http.DefaultClient, ctjsonclient.Options{Logger: logger})
	if err != nil {
		return err
	}

	// Never request more than MaxToRequest, else we get surprised by a massive server response
	entries, err := lc.GetRawEntries(context.Background(), int64(md.Start), minInt64(int64(md.End)-1, int64(md.Start)+MaxToRequest))
	if err != nil {
		return err
	}

	idx := md.Start
	for _, e := range entries.Entries {
		var leaf ct.MerkleTreeLeaf
		_, err := cttls.Unmarshal(e.LeafInput, &leaf)
		if err != nil {
			return err
		}
		if leaf.LeafType != ct.TimestampedEntryLeafType {
			return fmt.Errorf("unknown leaf type: %v", leaf.LeafType)
		}
		if leaf.TimestampedEntry == nil {
			return errors.New("nil timestamped entry")
		}
		var cert *ctx509.Certificate
		switch leaf.TimestampedEntry.EntryType {
		case ct.X509LogEntryType:
			// swallow errors, as this parser is will still return partially valid certs, which are good enough for our analysis
			cert, _ = leaf.X509Certificate()
			if cert == nil {
				_, err := tx.Exec("INSERT INTO error_log (error) VALUES ($1)", fmt.Sprintf("cannotparse|%s|%d", md.URL, idx))
				if err != nil {
					return err
				}
			}
		case ct.PrecertLogEntryType:
			// swallow errors, as this parser is will still return partially valid certs, which are good enough for our analysis
			cert, _ = leaf.Precertificate()
			if cert == nil {
				_, err := tx.Exec("INSERT INTO error_log (error) VALUES ($1)", fmt.Sprintf("cannotparse|%s|%d", md.URL, idx))
				if err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unknown leaf type: %v", leaf.LeafType)
		}

		doms := make(map[string]bool)

		if cert != nil {
			if strings.HasSuffix(cert.Subject.CommonName, DomainSuffix) || cert.Subject.CommonName == MatchDomain {
				doms[cert.Subject.CommonName] = true
			}

			for _, name := range cert.DNSNames {
				if strings.HasSuffix(name, DomainSuffix) || name == MatchDomain {
					doms[name] = true
				}
			}
		}

		if len(doms) != 0 {
			// We care more about the certs, than the logs, so let's wipe out the timestamp, so that
			// multiple logs reporting the same cert, only store one.
			// Now note that we'll still get some dupes, as a pre-cert and cert will appear as two different
			// things. TODO...
			leaf.TimestampedEntry.Timestamp = 0

			certToStore, err := cttls.Marshal(leaf)
			if err != nil {
				return err
			}

			kh := sha256.Sum256(certToStore)

			fields := []string{"key", "leaf"}
			ph := []string{"$1", "$2"}
			vals := []interface{}{kh[:], certToStore}
			var issuer string
			for k, v := range getFieldsAndValsForCert(&leaf) {
				fields = append(fields, k)
				ph = append(ph, fmt.Sprintf("$%d", len(ph)+1))
				vals = append(vals, v)
				if k == "issuer_cn" {
					issuer = v.(string)
				}
			}

			rows, err := tx.Query(fmt.Sprintf("INSERT INTO cert_store (%s) VALUES (%s) ON CONFLICT DO NOTHING RETURNING key", strings.Join(fields, ", "), strings.Join(ph, ", ")), vals...)
			if err != nil {
				return err
			}
			didInsert := rows.Next()
			rows.Close()

			var domList []string
			for dom := range doms {
				_, err = tx.Exec("INSERT INTO cert_index (key, domain) VALUES ($1, $2) ON CONFLICT DO NOTHING", kh[:], dom)
				if err != nil {
					return err
				}
				domList = append(domList, dom)
			}

			if didInsert {
				bb, err := json.Marshal(&UpdateSlackConf{
					Key:     base64.RawURLEncoding.EncodeToString(kh[:]),
					Domains: domList,
					Issuer:  issuer,
				})
				if err != nil {
					return err
				}
				err = qc.EnqueueInTx(&que.Job{
					Type: KeyUpdateSlack,
					Args: bb,
				}, tx)
				if err != nil {
					return err
				}

				bb, err = json.Marshal(&UpdateDataGovAUConf{
					Data: certToStore,
				})
				if err != nil {
					return err
				}
				err = qc.EnqueueInTx(&que.Job{
					Type: KeyUpdateDataGovAU,
					Args: bb,
				}, tx)
				if err != nil {
					return err
				}
			}
		}

		idx++
	}

	// Did we fall short of the amount we needed?
	if idx < md.End {
		midPoint := idx + ((md.End - idx) / 2)

		if idx < midPoint {
			bb, err := json.Marshal(&GetEntriesConf{
				URL:   md.URL,
				Start: idx,
				End:   midPoint,
			})
			if err != nil {
				return err
			}
			err = qc.EnqueueInTx(&que.Job{
				Type: KeyGetEntries,
				Args: bb,
			}, tx)
			if err != nil {
				return err
			}
		}

		if midPoint < md.End {
			bb, err := json.Marshal(&GetEntriesConf{
				URL:   md.URL,
				Start: midPoint,
				End:   md.End,
			})
			if err != nil {
				return err
			}
			err = qc.EnqueueInTx(&que.Job{
				Type: KeyGetEntries,
				Args: bb,
			}, tx)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
