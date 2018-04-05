package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx"

	"github.com/bgentry/que-go"
	cfenv "github.com/cloudfoundry-community/go-cfenv"

	"github.com/govau/certwatch/jobs"
	"github.com/govau/cf-common/env"
	commonjobs "github.com/govau/cf-common/jobs"
)

func main() {
	app, err := cfenv.Current()
	if err != nil {
		log.Fatal(err)
	}
	envLookup := env.NewVarSet(
		env.WithOSLookup(), // Always look in the OS env first.
		env.WithUPSLookup(app, "certwatch-ups"),
	)

	dataGovAU := &jobs.UpdateDataGovAU{
		APIKey:     envLookup.String("CKAN_API_KEY", ""),
		BaseURL:    envLookup.String("CKAN_BASE_URL", "https://data.gov.au"),
		ResourceID: envLookup.String("CKAN_RESOURCE_ID", ""),
	}

	log.Fatal((&commonjobs.Handler{
		PGXConnConfig: commonjobs.MustPGXConfigFromCloudFoundry(),
		WorkerCount:   5,
		WorkerMap: map[string]*commonjobs.JobConfig{
			jobs.KeyUpdateLogs: &commonjobs.JobConfig{
				F:         jobs.UpdateCTLogList,
				Singleton: true,
				Duration:  time.Hour * 24,
			},
			jobs.KeyNewLogMetadata: &commonjobs.JobConfig{
				F: jobs.NewLogMetadata,
			},
			jobs.KeyCheckSTH: &commonjobs.JobConfig{
				F:         jobs.CheckLogSTH,
				Singleton: true,
				Duration:  time.Minute * 5,
			},
			jobs.KeyGetEntries: &commonjobs.JobConfig{
				F: jobs.GetEntries,
			},
			jobs.KeyUpdateSlack: &commonjobs.JobConfig{
				F: (&jobs.UpdateSlack{
					Hook:    envLookup.String("SLACK_HOOK", ""),
					BaseURL: envLookup.String("BASE_METRICS_URL", ""),
				}).Run,
			},
			jobs.KeyUpdateDataGovAU: &commonjobs.JobConfig{
				F: dataGovAU.Run,
			},
			jobs.KeyBackfillDataGovAU: &commonjobs.JobConfig{
				F: dataGovAU.BackfillDataGovAU,
			},
			jobs.KeyUpdateMetadata: &commonjobs.JobConfig{
				F:         jobs.RefreshMetadataForEntries,
				Singleton: true,
			},
		},
		OnStart: func(qc *que.Client, pgxPool *pgx.ConnPool, logger *log.Logger) error {
			err := qc.Enqueue(&que.Job{
				Type:  jobs.KeyUpdateLogs,
				Args:  []byte("{}"),
				RunAt: time.Now(),
			})
			if err != nil {
				return err
			}

			// Handles migration
			err = qc.Enqueue(&que.Job{
				Type:  jobs.KeyUpdateMetadata,
				Args:  []byte("{}"),
				RunAt: time.Now(),
			})
			if err != nil {
				return err
			}

			logger.Println("Starting up... waiting for ctrl-C to stop.")
			go http.ListenAndServe(fmt.Sprintf(":%s", os.Getenv("PORT")), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, "Healthy")
			}))

			return nil
		},
		InitSQL: `
			CREATE TABLE IF NOT EXISTS cron_metadata (
				id             text                     PRIMARY KEY,
				last_completed timestamp with time zone NOT NULL DEFAULT TIMESTAMP 'EPOCH',
				next_scheduled timestamp with time zone NOT NULL DEFAULT TIMESTAMP 'EPOCH'
			);

			CREATE TABLE IF NOT EXISTS monitored_logs (
				url       text      PRIMARY KEY,
				processed bigint    NOT NULL DEFAULT 0,
				state     integer   NOT NULL DEFAULT 0,
				connect_url text
			);

			CREATE TABLE IF NOT EXISTS cert_store (
				key              bytea                     PRIMARY KEY,
				leaf             bytea                     NOT NULL,
				not_valid_before timestamp with time zone,
				not_valid_after  timestamp with time zone,
				issuer_cn        text,
				jurisdiction     text,
				cdn              text,
				needs_update     boolean,
				discovered       timestamptz               NOT NULL DEFAULT now(),
				needs_ckan_backfill boolean
			);

			CREATE TABLE IF NOT EXISTS cert_index (
				key          bytea         NOT NULL,
				domain       text          NOT NULL,

				CONSTRAINT cert_index_pkey PRIMARY KEY (key, domain)
			);

			CREATE TABLE IF NOT EXISTS error_log (
				discovered   timestamptz   NOT NULL DEFAULT now(),
				error        text          NOT NULL
			);
		`,
	}).WorkForever())
}
