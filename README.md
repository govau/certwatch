# Certwatch

This application is designed to monitor [Certificate Transparency](https://www.certificate-transparency.org/) logs and to find any new certificates issued for domain suffix and add them to a Postgresql database.

Specifically, once every 24 hours it will fetch the latest list of [known CT logs](https://www.gstatic.com/ct/log_list/all_logs_list.json) from Google (see [`jobs/job_update_logs.go`](./jobs/job_update_logs.go)) and set up a "cron" such that every 5 minutes a new signed tree head will be fetched (see [`jobs/job_check_sth.go`](./jobs/job_check_sth.go)), and if the tree size has increased, a job will be scheduled for fetch new entries (see [`jobs/job_get_entries.go`](./jobs/job_get_entries.go)).

The fetch entries job will try to fetch up to around 1000 entries at once, and if for any reason not all entries are returned (which is permitted per [RFC6962](https://tools.ietf.org/html/rfc6962)), it will reschedule 2 new jobs to fetch half of the remaining entries each.

If any requests fail, they will be retried using the `que-go` library, which handles exponential back-off.

Once new certificates of interest are detected, they are written to the Postgresql database, and (if configured) will send a notification to Slack hook, and (if configured) will add an entry to a CKAN data source (such as [data.gov.au](https://data.gov.au)).

## Design

This application is designed to run in CloudFoundry, and we host our instance on [cloud.gov.au](https://cloud.gov.au).

It connects to a Postgresql instance, and uses the [bgentry/que-go](https://github.com/bgentry/que-go) library to manage multiple worker threads using Postgres as a locking mechanism.

The `certwatch` application does the main work, and is designed to safely run as many as you wish concurrently.

The `certmetrics` application simply exposes Promethethus metrics, and provides an endpoint to view certificates as referenced via links sent to the Slack webhook. Since this app runs regular database queries for gathering metrics, you probably don't want to run more than 1, and don't need to run any if you don't use Promethetheus or the Slack webhooks.

## Running locally

```bash
docker run -p 5434:5432 --name certpg -e POSTGRES_USER=certpg -e POSTGRES_PASSWORD=certpg -d postgres

export VCAP_APPLICATION='{}'
export VCAP_SERVICES='{"postgres": [{"credentials": {"username": "certpg", "host": "localhost", "password": "certpg", "name": "certpg", "port": 5434}, "tags": ["postgres"]}]}'
export PORT=8080

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

## Build and push to CloudFoundry

```bash
cf create-service postgres 9.6-5G govaucerts
cf create-user-provided-service certwatch-ups -p '{"SLACK_HOOK":"https://hooks.slack.com/services/xxx","BASE_METRICS_URL":"https://certmetrics.apps.y.cld.gov.au"}'

# Build and push
dep ensure
GOOS=linux GOARCH=amd64 go build -o cf/certwatch/certwatch cmd/certwatch/main.go
cf push -f cf/certwatch/manifest.yml -p cf/certwatch

# Metric server (optional)
GOOS=linux GOARCH=amd64 go build -o cf/certmetrics/certmetrics cmd/certmetrics/metrics-main.go
cf push -f cf/certmetrics/manifest.yml -p cf/certmetrics
```

## Useful SQL commands

Since we are using a Postgres table to manage queues, the application can be controlled by sending various commands. e.g.

```sql
-- To re-run the indexing of useful fields, e.g. if logic is added:
update cert_store set needs_update=true;
insert into que_jobs(job_class,args) values('update_metadata','{}');

-- To backfill into a CKAN dataset:
update cert_store set needs_ckan_backfill=true;
insert into que_jobs(job_class,args) values('backfill_data_gov_au','{}');

-- To add a new log for processing:
insert into que_jobs(job_class,args) values('new_log_metadata','{"url":"ct.googleapis.com/daedalus/"}') on conflict do nothing;

-- To disable scanning a log
update monitored_logs set state = 1 where url = 'ct.googleapis.com/daedalus/';

-- To show all errors
select * from que_jobs where error_count != 0;
```
