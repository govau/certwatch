package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/govau/certwatch/db"
)

var (
	queJobs = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "que_jobs",
		Help: "number of jobs outstanding",
	})
	jobsWithErrors = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "jobs_with_errors",
		Help: "number of jobs with errors",
	})
	remainingEntries = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "remaining_entries",
		Help: "total entries backlogged",
	})
	processedEntries = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "processed_entries",
		Help: "total entries processed",
	})
)

func init() {
	// Metrics have to be registered to be exposed:
	prometheus.MustRegister(queJobs)
	prometheus.MustRegister(jobsWithErrors)
	prometheus.MustRegister(remainingEntries)
	prometheus.MustRegister(processedEntries)
}

func updateStatLoop() {
	pgxPool, err := db.GetPGXPool(1)
	if err != nil {
		log.Fatal(err)
	}
	defer pgxPool.Close()

	for {
		var i int

		err := pgxPool.QueryRow("SELECT COUNT(*) FROM que_jobs").Scan(&i)
		if err != nil {
			log.Println(err)
		} else {
			queJobs.Set(float64(i))
		}

		err = pgxPool.QueryRow("SELECT COUNT(*) FROM que_jobs WHERE error_count != 0").Scan(&i)
		if err != nil {
			log.Println(err)
		} else {
			jobsWithErrors.Set(float64(i))
		}

		var knownEntries int64
		err = pgxPool.QueryRow("SELECT SUM(processed) FROM monitored_logs").Scan(&knownEntries)
		if err != nil {
			log.Println(err)
		} else {
			var remEntries int64
			err = pgxPool.QueryRow("SELECT SUM((args->>'End')::int - (args->>'Start')::int) FROM que_jobs WHERE job_class = 'get_entries'").Scan(&remEntries)
			if err != nil {
				log.Println(err)
			} else {
				remainingEntries.Set(float64(remEntries))
				processedEntries.Set(float64(knownEntries - remEntries))
			}

		}

		time.Sleep(time.Second * 30)
	}
}

func main() {
	go updateStatLoop()

	log.Println("Started up... waiting for ctrl-C.")

	http.Handle("/metrics", promhttp.Handler())
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", os.Getenv("PORT")), nil))
}
