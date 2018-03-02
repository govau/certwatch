# Running locally

```bash
docker run -p 5434:5432 --name certpg -e POSTGRES_USER=certpg -e POSTGRES_PASSWORD=certpg -d postgres

export VCAP_APPLICATION='{}'
export VCAP_SERVICES='{"postgres": [{"credentials": {"username": "certpg", "host": "localhost", "password": "certpg", "name": "certpg", "port": 5434}, "tags": ["postgres"]}]}'

# Optional
export SLACK_HOOK="https://hooks.slack.com/services/xxx"
export BASE_METRICS_URL="http://localhost:4323"

# Optional - send copy to a CKAN:
export CKAN_API_KEY="xxx"
export CKAN_RESOURCE_ID="xxx"
export CKAN_BASE_URL="https://data.gov.au"

go run cmd/certwatch/main.go
```

To checkout database:

```bash
psql "dbname=certpg host=localhost user=certpg password=certpg port=5434"
```

Build and push:

```bash
cfy create-service postgres 9.6-5G govaucerts
cfy create-user-provided-service certwatch-ups -p '{"SLACK_HOOK":"https://hooks.slack.com/services/xxx","BASE_METRICS_URL":"https://certmetrics.apps.y.cld.gov.au"}'

# Build and push
dep ensure
GOOS=linux GOARCH=amd64 go build -o cf/certwatch/certwatch cmd/certwatch/main.go
cfy push -f cf/certwatch/manifest.yml -p cf/certwatch

# Tell it to update:
# update cert_store set needs_update=true
# insert into que_jobs(job_class,args) values('update_metadata','{}')
# update cert_store set needs_ckan_backfill=true
# insert into que_jobs(job_class,args) values('backfill_data_gov_au','{}')
#
# To add a new job for processing: insert into que_jobs(job_class,args) values('new_log_metadata','{"url":"ct.googleapis.com/daedalus/"}') on conflict do nothing;
cfy restart certwatch


# Metrics
GOOS=linux GOARCH=amd64 go build -o cf/certmetrics/certmetrics cmd/certmetrics/metrics-main.go
cfy push -f cf/certmetrics/manifest.yml -p cf/certmetrics
```