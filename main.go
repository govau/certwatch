package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	cfenv "github.com/cloudfoundry-community/go-cfenv"

	"github.com/bgentry/que-go"
	"github.com/jackc/pgx"
)

type server struct {
}

// Return a database object, using the CloudFoundry environment data
func postgresCredsFromCF() (map[string]interface{}, error) {
	appEnv, err := cfenv.Current()
	if err != nil {
		return nil, err
	}

	dbEnv, err := appEnv.Services.WithTag("postgres")
	if err != nil {
		return nil, err
	}

	if len(dbEnv) != 1 {
		return nil, errors.New("expecting 1 database")
	}

	return dbEnv[0].Credentials, nil
}

const (
	WorkerCount = 5
)

type DBInitter struct {
	InitSQL            string
	PreparedStatements map[string]string
	OtherStatements    func(*pgx.Conn) error

	// Clearly this won't stop other instances in a race condition, but should at least stop ourselves from hammering ourselves unnecessarily
	runMutex   sync.Mutex
	runAlready bool
}

func (dbi *DBInitter) ensureInitDone(c *pgx.Conn) error {
	dbi.runMutex.Lock()
	defer dbi.runMutex.Unlock()

	if dbi.runAlready {
		return nil
	}

	_, err := c.Exec(dbi.InitSQL)
	if err != nil {
		return err
	}

	dbi.runAlready = true
	return nil
}

func (dbi *DBInitter) AfterConnect(c *pgx.Conn) error {
	if dbi.InitSQL != "" {
		err := dbi.ensureInitDone(c)
		if err != nil {
			return err
		}
	}

	if dbi.OtherStatements != nil {
		err := dbi.OtherStatements(c)
		if err != nil {
			return err
		}
	}

	if dbi.PreparedStatements != nil {
		for n, sql := range dbi.PreparedStatements {
			_, err := c.Prepare(n, sql)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func main() {
	creds, err := postgresCredsFromCF()
	if err != nil {
		log.Fatal(err)
	}

	pgxPool, err := pgx.NewConnPool(pgx.ConnPoolConfig{
		MaxConnections: WorkerCount * 2,
		ConnConfig: pgx.ConnConfig{
			Database: creds["name"].(string),
			User:     creds["username"].(string),
			Password: creds["password"].(string),
			Host:     creds["host"].(string),
			Port:     uint16(creds["port"].(float64)),
		},
		AfterConnect: (&DBInitter{
			InitSQL: `
				CREATE TABLE IF NOT EXISTS que_jobs (
					priority    smallint    NOT NULL DEFAULT 100,
					run_at      timestamptz NOT NULL DEFAULT now(),
					job_id      bigserial   NOT NULL,
					job_class   text        NOT NULL,
					args        json        NOT NULL DEFAULT '[]'::json,
					error_count integer     NOT NULL DEFAULT 0,
					last_error  text,
					queue       text        NOT NULL DEFAULT '',

					CONSTRAINT que_jobs_pkey PRIMARY KEY (queue, priority, run_at, job_id)
				);

				COMMENT ON TABLE que_jobs IS '3';

				CREATE TABLE IF NOT EXISTS cron_metadata (
					id             text                     PRIMARY KEY,
					last_completed timestamp with time zone NOT NULL DEFAULT TIMESTAMP 'EPOCH',
					next_scheduled timestamp with time zone NOT NULL DEFAULT TIMESTAMP 'EPOCH'
				);

				CREATE TABLE IF NOT EXISTS monitored_logs (
					url       text      PRIMARY KEY,
					processed bigint    NOT NULL DEFAULT 0,
					state     integer   NOT NULL DEFAULT 0
				);
					
				CREATE TABLE IF NOT EXISTS cert_store (
					key          bytea         PRIMARY KEY,
					leaf         bytea         NOT NULL
				);

				CREATE TABLE IF NOT EXISTS cert_index (
					key          bytea         NOT NULL,
					domain       text          NOT NULL,
					discovered   timestamptz   NOT NULL DEFAULT now(),

					CONSTRAINT cert_index_pkey PRIMARY KEY (key, domain)
				);

				CREATE TABLE IF NOT EXISTS error_log (
					discovered   timestamptz   NOT NULL DEFAULT now(),
					error        text          NOT NULL
				);
				`,
			OtherStatements:    que.PrepareStatements,
			PreparedStatements: map[string]string{},
		}).AfterConnect,
	})
	if err != nil {
		log.Fatal(err)
	}

	qc := que.NewClient(pgxPool)
	workers := que.NewWorkerPool(qc, que.WorkMap{
		KeyUpdateLogs: (&JobFuncWrapper{
			QC:        qc,
			Logger:    log.New(os.Stderr, KeyUpdateLogs+" ", log.LstdFlags),
			F:         UpdateCTLogList,
			Singleton: true,
			Duration:  time.Hour * 24,
		}).Run,
		KeyNewLogMetadata: (&JobFuncWrapper{
			QC:     qc,
			Logger: log.New(os.Stderr, KeyNewLogMetadata+" ", log.LstdFlags),
			F:      NewLogMetadata,
		}).Run,
		KeyCheckSTH: (&JobFuncWrapper{
			QC:        qc,
			Logger:    log.New(os.Stderr, KeyCheckSTH+" ", log.LstdFlags),
			F:         CheckLogSTH,
			Singleton: true,
			Duration:  time.Minute * 5,
		}).Run,
		KeyGetEntries: (&JobFuncWrapper{
			QC:     qc,
			Logger: log.New(os.Stderr, KeyGetEntries+" ", log.LstdFlags),
			F:      GetEntries,
		}).Run,
	}, WorkerCount)

	// Prepare a shutdown function
	shutdown := func() {
		workers.Shutdown()
		pgxPool.Close()
	}

	// Normal exit
	defer shutdown()

	// Or via signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	signal.Notify(sigCh, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("Received %v, starting shutdown...", sig)
		shutdown()
		log.Println("Shutdown complete")
		os.Exit(0)
	}()

	go workers.Start()

	err = qc.Enqueue(&que.Job{
		Type:  KeyUpdateLogs,
		Args:  []byte("{}"),
		RunAt: time.Now(),
	})
	if err != nil {
		log.Fatal(err)
	}

	log.Println("Started up... waiting for ctrl-C.")

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Up and away.")
	})
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", os.Getenv("PORT")), nil))
}
