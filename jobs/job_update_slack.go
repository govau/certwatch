package jobs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	que "github.com/bgentry/que-go"
	"github.com/jackc/pgx"
)

const (
	KeyUpdateSlack = "cron_slack"
)

type UpdateSlackConf struct {
	Key     string
	Domains []string
	Issuer  string
}

type UpdateSlack struct {
	BaseURL string
	Hook    string
}

func (us *UpdateSlack) Run(qc *que.Client, logger *log.Logger, job *que.Job, tx *pgx.Tx) error {
	// If we don't use slack, fail fast
	if us.Hook == "" {
		return nil
	}

	var conf UpdateSlackConf
	err := json.Unmarshal(job.Args, &conf)
	if err != nil {
		return err
	}

	payload, err := json.Marshal(&struct {
		Text string `json:"text"`
	}{
		Text: fmt.Sprintf("*%s* <%s/cert/%s|View...>\n```%s```\n", conf.Issuer, us.BaseURL, conf.Key, strings.Join(conf.Domains, "\n")),
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, us.Hook, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return nil

	case http.StatusTooManyRequests:
		// Come back later
		ttl, err := strconv.Atoi(resp.Header.Get("Retry-After"))
		if err != nil {
			return err
		}
		return qc.EnqueueInTx(&que.Job{
			Type:  KeyUpdateSlack,
			Args:  job.Args,
			RunAt: time.Now().Add(time.Duration(ttl) * time.Second),
		}, tx)
	default:
		return fmt.Errorf("bad status from Slack: %v", resp.StatusCode)
	}
}
