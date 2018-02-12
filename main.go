package main

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	cfenv "github.com/cloudfoundry-community/go-cfenv"
	"github.com/go-pg/pg"
	"github.com/gorilla/websocket"
)

type Certificate struct {
	Subject struct {
		Aggregated       string `json:"aggregated"`
		Country          string `json:"C"`
		State            string `json:"ST"`
		Locality         string `json:"L"`
		Organisation     string `json:"O"`
		OrganisationUnit string `json:"OU"`
		CommonName       string `json:"CN"`
	} `json:"subject"`
	Extensions struct {
		KeyUsage               string `json:"keyUsage"`
		ExtendedKeyUsage       string `json:"extendedKeyUsage"`
		BasicConstraints       string `json:"basicConstraints"`
		SubjectKeyIdentifier   string `json:"subjectKeyIdentifier"`
		AuthorityKeyIdentifier string `json:"authorityKeyIdentifier"`
		AuthorityInfoAccess    string `json:"authorityInfoAccess"`
		SubjectAltName         string `json:"subjectAltName"`
		CertificatePolicies    string `json:"certificatePolicies"`
		CRLDistributionPoints  string `json:"crlDistributionPoints"`
	} `json:"extensions"`
	NotBefore    float64  `json:"not_before"`
	NotAfter     float64  `json:"not_after"`
	SerialNumber string   `json:"serial_number"`
	Fingerprint  string   `json:"fingerprint"`
	DER          string   `json:"as_der"`
	AllDomains   []string `json:"all_domains"`
}

type Message struct {
	MessageType string `json:"message_type"`
	Data        struct {
		UpdateType string        `json:"update_type"`
		LeafCert   Certificate   `json:"leaf_cert"`
		Chain      []Certificate `json:"chain"`
	} `json:data`
	CertIndex int64   `json:"cert_index"`
	Seen      float64 `json:"seen"`
	Source    struct {
		URL  string `json:"url"`
		Name string `json:"name"`
	} `json:"source"`
}

func (s *server) StreamAndLog() {
	for {
		c, _, err := websocket.DefaultDialer.Dial("wss://certstream.calidog.io", nil)

		if err != nil {
			log.Println("Error connecting to certstream! Sleeping a few seconds and reconnecting... ")
			time.Sleep(5 * time.Second)
			continue
		}

		for {
			var message Message
			err := c.ReadJSON(&message)
			if err != nil {
				log.Println("Error reading message")
				break
			}

			if message.MessageType == "certificate_update" {
				err = s.gotCert(&message.Data.LeafCert)
				if err != nil {
					log.Println("error having cert", err)
				}
			}
		}

		c.Close()
	}
}

type CertObserved struct {
	Domain       string `sql:",pk"`
	SerialNumber string `sql:",pk"`
	Seen         time.Time
}

type server struct {
	DB *pg.DB
}

// Return a database object, using the CloudFoundry environment data
func postgresDBFromCF() (*pg.DB, error) {
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

	return pg.Connect(&pg.Options{
		Addr:     fmt.Sprintf("%s:%d", dbEnv[0].Credentials["host"].(string), int(dbEnv[0].Credentials["port"].(float64))),
		Database: dbEnv[0].Credentials["name"].(string),
		User:     dbEnv[0].Credentials["username"].(string),
		Password: dbEnv[0].Credentials["password"].(string),
	}), nil
}

func (s *server) gotCert(cert *Certificate) error {
	interesting := false
	for _, dom := range cert.AllDomains {
		if strings.HasSuffix(dom, ".gov.au") {
			interesting = true
			break
		}
	}

	if interesting {
		for _, dom := range cert.AllDomains {
			err := s.DB.Insert(&CertObserved{
				Domain:       dom,
				SerialNumber: cert.SerialNumber,
				Seen:         time.Now(),
			})
			if err != nil {
				// TOOD: catch specific error: ERROR #23505 duplicate key value violates unique constraint "cert_observeds_pkey" (addr="[::1]:5434")
				return err
			}
		}
	}

	return nil
}

func (s *server) Init() error {
	for _, model := range []interface{}{
		&CertObserved{},
	} {
		// Ignore failures here - the table probably already exists TODO catch specific erro: ERROR #42P07 relation "cert_observeds" already exists (addr="[::1]:5434")
		s.DB.CreateTable(model, nil)
	}
	return nil
}

func main() {
	db, err := postgresDBFromCF()
	if err != nil {
		log.Fatal(err)
	}

	s := &server{
		DB: db,
	}
	err = s.Init()
	if err != nil {
		log.Fatal(err)
	}

	s.StreamAndLog()
}
