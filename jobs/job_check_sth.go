package jobs

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	que "github.com/bgentry/que-go"
	ctclient "github.com/google/certificate-transparency-go/client"
	ctjsonclient "github.com/google/certificate-transparency-go/jsonclient"
	"github.com/jackc/pgx"
)

// CheckSTHConf is stored in the que_jobs table
type CheckSTHConf struct {
	// URL is used to lookup the record in the monitored_logs table
	URL string
}

const (
	// KeyCheckSTH is the name of the job
	KeyCheckSTH = "cron_check_sth"

	// InsecurePrefix can be prepended to the "connect_url" field to indicate that TLS verification should be disabled for a particular URL.
	// This is used from some older logs that appear to still be up, but have issues with their certificates.
	InsecurePrefix = "insecure-skip-verify-"
)

func makeClientForURL(connectURL string) (string, *http.Client) {
	if strings.HasPrefix(connectURL, InsecurePrefix) {
		return connectURL[len(InsecurePrefix):], &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,
				},
			},
		}
	}

	return connectURL, http.DefaultClient
}

// CheckLogSTH checks for new entries, and schedules a job to fetch them if needed
func CheckLogSTH(qc *que.Client, logger *log.Logger, job *que.Job, tx *pgx.Tx) error {
	var md CheckSTHConf
	err := json.Unmarshal(job.Args, &md)
	if err != nil {
		return err
	}

	// Get a lock, though we should already have one via other means
	var state int
	var processed uint64
	var connectURL string

	// ensure state is active, else return error
	err = tx.QueryRow("SELECT state, processed, connect_url FROM monitored_logs WHERE url = $1 FOR UPDATE", md.URL).Scan(&state, &processed, &connectURL)
	if err != nil {
		return err
	}

	// Don't reschedule us as a cron please.
	if state != StateActive {
		return ErrDidNotReschedule
	}

	url, client := makeClientForURL(connectURL)
	lc, err := ctclient.New(url, client, ctjsonclient.Options{Logger: logger})
	if err != nil {
		return err
	}

	sth, err := lc.GetSTH(context.Background())
	if err != nil {
		return err
	}

	if sth.TreeSize > processed {
		// We have work to do!
		bb, err := json.Marshal(&GetEntriesConf{
			URL:   connectURL,
			Start: processed,
			End:   sth.TreeSize,
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
		_, err = tx.Exec("UPDATE monitored_logs SET processed = $1 WHERE url = $2", sth.TreeSize, md.URL)
		if err != nil {
			return err
		}
	}
	return nil
}
