package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bgentry/que-go"
	cfenv "github.com/cloudfoundry-community/go-cfenv"

	"github.com/govau/certwatch/db"
	"github.com/govau/certwatch/jobs"
	"github.com/govau/cf-common/env"
)

const (
	WorkerCount = 5
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

	pgxPool, err := db.GetPGXPool(WorkerCount * 2)
	if err != nil {
		log.Fatal(err)
	}

	qc := que.NewClient(pgxPool)
	workers := que.NewWorkerPool(qc, que.WorkMap{
		jobs.KeyUpdateLogs: (&jobs.JobFuncWrapper{
			QC:        qc,
			Logger:    log.New(os.Stderr, jobs.KeyUpdateLogs+" ", log.LstdFlags),
			F:         jobs.UpdateCTLogList,
			Singleton: true,
			Duration:  time.Hour * 24,
		}).Run,
		jobs.KeyNewLogMetadata: (&jobs.JobFuncWrapper{
			QC:     qc,
			Logger: log.New(os.Stderr, jobs.KeyNewLogMetadata+" ", log.LstdFlags),
			F:      jobs.NewLogMetadata,
		}).Run,
		jobs.KeyCheckSTH: (&jobs.JobFuncWrapper{
			QC:        qc,
			Logger:    log.New(os.Stderr, jobs.KeyCheckSTH+" ", log.LstdFlags),
			F:         jobs.CheckLogSTH,
			Singleton: true,
			Duration:  time.Minute * 5,
		}).Run,
		jobs.KeyGetEntries: (&jobs.JobFuncWrapper{
			QC:     qc,
			Logger: log.New(os.Stderr, jobs.KeyGetEntries+" ", log.LstdFlags),
			F:      jobs.GetEntries,
		}).Run,
		jobs.KeyUpdateSlack: (&jobs.JobFuncWrapper{
			QC:     qc,
			Logger: log.New(os.Stderr, jobs.KeyUpdateSlack+" ", log.LstdFlags),
			F: (&jobs.UpdateSlack{
				Hook:    envLookup.String("SLACK_HOOK", ""),
				BaseURL: envLookup.String("BASE_METRICS_URL", ""),
			}).Run,
		}).Run,
		jobs.KeyUpdateMetadata: (&jobs.JobFuncWrapper{
			QC:        qc,
			Logger:    log.New(os.Stderr, jobs.KeyUpdateMetadata+" ", log.LstdFlags),
			F:         jobs.RefreshMetadataForEntries,
			Singleton: true,
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
		Type:  jobs.KeyUpdateLogs,
		Args:  []byte("{}"),
		RunAt: time.Now(),
	})
	if err != nil {
		log.Fatal(err)
	}

	// Handles migration
	err = qc.Enqueue(&que.Job{
		Type:  jobs.KeyUpdateMetadata,
		Args:  []byte("{}"),
		RunAt: time.Now(),
	})
	if err != nil {
		log.Fatal(err)
	}

	// Was used for data migration, no longer needed
	// err = qc.Enqueue(&que.Job{
	// 	Type:  jobs.KeyFixMetadata1,
	// 	Args:  []byte("{}"),
	// 	RunAt: time.Now(),
	// })
	// if err != nil {
	// 	log.Fatal(err)
	// }

	log.Println("Started up... waiting for ctrl-C.")

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Up and away.")
	})
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", os.Getenv("PORT")), nil))
}
