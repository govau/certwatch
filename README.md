# Running locally

```bash
docker run -p 5434:5432 --name certpg -e POSTGRES_USER=certpg -e POSTGRES_PASSWORD=certpg -d postgres

export VCAP_APPLICATION='{}'
export VCAP_SERVICES='{"postgres": [{"credentials": {"username": "certpg", "host": "localhost", "password": "certpg", "name": "certpg", "port": 5434}, "tags": ["postgres"]}]}'
go run *.go
```

To checkout database:

```bash
psql "dbname=certpg host=localhost user=certpg password=certpg port=5434"
```

Build and push:

```bash
cfy create-service postgres 9.6-5G govaucerts

# Build and push
dep ensure
GOOS=linux GOARCH=amd64 go build -o cf/certwatch/certwatch cmd/certwatch/main.go
cfy push -f cf/certwatch/manifest.yml -p cf/certwatch

# Metrics
GOOS=linux GOARCH=amd64 go build -o cf/certmetrics/certmetrics cmd/certmetrics/metrics-main.go
cfy push -f cf/certmetrics/manifest.yml -p cf/certmetrics
```