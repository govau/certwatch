package main

import (
	"encoding/json"
	"log"

	que "github.com/bgentry/que-go"
	"github.com/jackc/pgx"
)

const (
	KeyNewLogMetadata = "new_log_metadata"
)

func NewLogMetadata(qc *que.Client, logger *log.Logger, job *que.Job, tx *pgx.Tx) error {
	var l CTLog
	err := json.Unmarshal(job.Args, &l)
	if err != nil {
		return err
	}

	var dbState int
	err = tx.QueryRow("SELECT state FROM monitored_logs WHERE url = $1 FOR UPDATE", l.URL).Scan(&dbState)
	if err != nil {
		if err == pgx.ErrNoRows {
			_, err = tx.Exec("INSERT INTO monitored_logs (url) VALUES ($1)", l.URL)
			if err != nil {
				return err
			}
			err = tx.Commit()
			if err != nil {
				return err
			}
			return ErrTryAgainPlease
		}
		return err
	}

	if (l.FinalSTH != nil || l.DisqualifiedAt != 0) && dbState == StateActive {
		_, err = tx.Exec("UPDATE monitored_logs SET state = $1 WHERE url = $2", StateIgnore, l.URL)
		if err != nil {
			return err
		}
		dbState = StateIgnore
	}

	if dbState == StateActive {
		bb, err := json.Marshal(&CheckSTHConf{URL: l.URL})
		if err != nil {
			return err
		}
		err = qc.EnqueueInTx(&que.Job{
			Type: KeyCheckSTH,
			Args: bb,
		}, tx)
		if err != nil {
			return err
		}
	}

	return nil
}
