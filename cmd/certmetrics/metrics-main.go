package main

import (
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/govau/certwatch/db"

	ct "github.com/google/certificate-transparency-go"
	cttls "github.com/google/certificate-transparency-go/tls"
	ctx509 "github.com/google/certificate-transparency-go/x509"
	"github.com/google/certificate-transparency-go/x509util"
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
	activeCertsByCDN = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "active_certs_by_cdn",
		Help: "active certs by cdn (not expired)",
	}, []string{"jurisdiction", "cdn"})
	activeCertsByIssuer = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "active_certs_by_issuer",
		Help: "active certs by issuer (not expired)",
	}, []string{"jurisdiction", "issuer"})
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
	prometheus.MustRegister(activeCertsByCDN)
	prometheus.MustRegister(activeCertsByIssuer)
}

type server struct {
	DB *pgx.ConnPool
}

func (s *server) updateStatLoop() {
	for {
		var i int

		err := s.DB.QueryRow("SELECT COUNT(*) FROM que_jobs").Scan(&i)
		if err != nil {
			log.Println(err)
		} else {
			queJobs.Set(float64(i))
		}

		err = s.DB.QueryRow("SELECT COUNT(*) FROM que_jobs WHERE error_count != 0").Scan(&i)
		if err != nil {
			log.Println(err)
		} else {
			jobsWithErrors.Set(float64(i))
		}

		err = s.DB.QueryRow("SELECT COUNT(*) FROM cert_index").Scan(&i)
		if err != nil {
			log.Println(err)
		} else {
			certsFound.Set(float64(i))
		}

		err = s.DB.QueryRow("SELECT COUNT(*) FROM cert_store").Scan(&i)
		if err != nil {
			log.Println(err)
		} else {
			uniqueCertsFound.Set(float64(i))
		}

		err = s.DB.QueryRow("SELECT COUNT(*) FROM monitored_logs").Scan(&i)
		if err != nil {
			log.Println(err)
		} else {
			activeLogsMonitored.Set(float64(i))
		}

		rows, err := s.DB.Query(`SELECT l.processed, COALESCE(r.remaining, 0), l.url
			FROM monitored_logs l
			LEFT OUTER JOIN
			(SELECT args->>'URL' url, SUM((args->>'End')::int - (args->>'Start')::int) remaining FROM que_jobs WHERE job_class = 'get_entries' GROUP BY url) r
			ON l.url = r.url
		`)
		if err != nil {
			log.Println(err)
		} else {
			remainingEntries.Reset()
			processedEntries.Reset()
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

		rows, err = s.DB.Query(`select jurisdiction, cdn, count(*) from cert_store where not_valid_after > now() and not_valid_before < now() group by jurisdiction, cdn`)
		if err != nil {
			log.Println(err)
		} else {
			activeCertsByCDN.Reset()
			for rows.Next() {
				var count int64
				var jurisdiction, cdn string
				err = rows.Scan(&jurisdiction, &cdn, &count)
				if err != nil {
					log.Println(err)
					break
				}
				activeCertsByCDN.With(prometheus.Labels{"jurisdiction": jurisdiction, "cdn": cdn}).Set(float64(count))
			}
			rows.Close()
		}

		rows, err = s.DB.Query(`select issuer_cn, jurisdiction, count(*) from cert_store where not_valid_after > now() and not_valid_before < now() group by issuer_cn, jurisdiction`)
		if err != nil {
			log.Println(err)
		} else {
			activeCertsByIssuer.Reset()
			for rows.Next() {
				var count int64
				var issuer, jurisdiction string
				err = rows.Scan(&issuer, &jurisdiction, &count)
				if err != nil {
					log.Println(err)
					break
				}
				activeCertsByIssuer.With(prometheus.Labels{"issuer": issuer, "jurisdiction": jurisdiction}).Set(float64(count))
			}
			rows.Close()
		}

		time.Sleep(time.Second * 30)
	}
}

func (s *server) showCert(w http.ResponseWriter, r *http.Request) {
	key, err := base64.RawURLEncoding.DecodeString(mux.Vars(r)["key"])
	if err != nil {
		http.Error(w, "Bad key", http.StatusBadRequest)
		return
	}

	var data []byte
	err = s.DB.QueryRow("SELECT leaf FROM cert_store WHERE key = $1", key).Scan(&data)
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	var leaf ct.MerkleTreeLeaf
	_, err = cttls.Unmarshal(data, &leaf)
	if err != nil {
		http.Error(w, "Bad data", http.StatusInternalServerError)
		return
	}

	var cert *ctx509.Certificate
	switch leaf.TimestampedEntry.EntryType {
	case ct.X509LogEntryType:
		cert, _ = leaf.X509Certificate()
	case ct.PrecertLogEntryType:
		cert, _ = leaf.Precertificate()
	default:
		http.Error(w, "Bad data - 1", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(x509util.CertificateToString(cert)))
}

func main() {
	pgxPool, err := db.GetPGXPool(5)
	if err != nil {
		log.Fatal(err)
	}
	defer pgxPool.Close()

	s := &server{
		DB: pgxPool,
	}

	go s.updateStatLoop()

	log.Println("Started up... waiting for ctrl-C.")

	r := mux.NewRouter()
	r.Handle("/metrics", promhttp.Handler())
	r.HandleFunc("/cert/{key}", s.showCert)

	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", os.Getenv("PORT")), r))
}
