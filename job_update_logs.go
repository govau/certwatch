package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/bgentry/que-go"
	"github.com/jackc/pgx"
)

const (
	StateActive = 0
	StateIgnore = 1
)

const (
	KeyUpdateLogs = "cron_update_logs"

	KnownLogsURL = "https://www.gstatic.com/ct/log_list/log_list.json"
)

type CTLog struct {
	URL            string `json:"url"`
	DisqualifiedAt int64  `json:"disqualified_at"`
	FinalSTH       *struct {
		TreeSize int64 `json:"tree_size"`
	} `json:"final_sth"`
}

func UpdateCTLogList(qc *que.Client, logger *log.Logger, job *que.Job, tx *pgx.Tx) error {
	resp, err := http.Get(KnownLogsURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return errors.New("bad status code")
	}

	var logData struct {
		Logs []*CTLog `json:"logs"`
	}

	err = json.NewDecoder(resp.Body).Decode(&logData)
	if err != nil {
		return err
	}

	for _, l := range logData.Logs {
		bb, err := json.Marshal(l)
		if err != nil {
			return err
		}
		err = qc.EnqueueInTx(&que.Job{
			Type: KeyNewLogMetadata,
			Args: bb,
		}, tx)
		if err != nil {
			return err
		}
	}

	return nil
}
