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
	remainingEntries = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "remaining_entries",
		Help: "total entries backlogged",
	}, []string{"log"})
	processedEntries = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "processed_entries",
		Help: "total entries processed",
	}, []string{"log"})
	certsFound = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "govau_certs_found",
		Help: "Total certs found (by domain)",
	})
	uniqueCertsFound = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "unique_govau_certs_found",
		Help: "Total unique cert timestamps found",
	})
	activeLogsMonitored = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "active_logs_monitored",
		Help: "Total active logs monitored",
	})
	activeCerts = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "active_certificates",
		Help: "active certs (not expired)",
	}, []string{"issuer"})
)

func init() {
	// Metrics have to be registered to be exposed:
	prometheus.MustRegister(queJobs)
	prometheus.MustRegister(jobsWithErrors)
	prometheus.MustRegister(remainingEntries)
	prometheus.MustRegister(processedEntries)
	prometheus.MustRegister(certsFound)
	prometheus.MustRegister(uniqueCertsFound)
	prometheus.MustRegister(activeLogsMonitored)
	prometheus.MustRegister(activeCerts)
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

		err = pgxPool.QueryRow("SELECT COUNT(*) FROM cert_index").Scan(&i)
		if err != nil {
			log.Println(err)
		} else {
			certsFound.Set(float64(i))
		}

		err = pgxPool.QueryRow("SELECT COUNT(*) FROM cert_store").Scan(&i)
		if err != nil {
			log.Println(err)
		} else {
			uniqueCertsFound.Set(float64(i))
		}

		err = pgxPool.QueryRow("SELECT COUNT(*) FROM monitored_logs").Scan(&i)
		if err != nil {
			log.Println(err)
		} else {
			activeLogsMonitored.Set(float64(i))
		}

		rows, err := pgxPool.Query(`SELECT l.processed, COALESCE(r.remaining, 0), l.url
			FROM monitored_logs l
			LEFT OUTER JOIN
			(SELECT args->>'URL' url, SUM((args->>'End')::int - (args->>'Start')::int) remaining FROM que_jobs WHERE job_class = 'get_entries' GROUP BY url) r
			ON l.url = r.url
		`)
		if err != nil {
			log.Println(err)
		} else {
			for rows.Next() {
				var processed, remaining int64
				var url string
				err = rows.Scan(&processed, &remaining, &url)
				if err != nil {
					log.Println(err)
					break
				}

				remainingEntries.With(prometheus.Labels{"log": url}).Set(float64(remaining))
				processedEntries.With(prometheus.Labels{"log": url}).Set(float64(processed - remaining))
			}
			rows.Close()
		}

		rows, err = pgxPool.Query(`select issuer_cn, count(*) from cert_store where not_valid_after > now() and not_valid_before < now() group by issuer_cn`)
		if err != nil {
			log.Println(err)
		} else {
			// TODO - if we now have zero, we'll get no results, and no way to delete the metric from prom?
			for rows.Next() {
				var count int64
				var issuer string
				err = rows.Scan(&issuer, &count)
				if err != nil {
					log.Println(err)
					break
				}
				activeCerts.With(prometheus.Labels{"issuer": issuer}).Set(float64(count))
			}
			rows.Close()
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
