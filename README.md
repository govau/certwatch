# Running locally

```bash
docker run -p 5434:5432 --name certpg -e POSTGRES_USER=certpg -e POSTGRES_PASSWORD=certpg -d postgres

export VCAP_APPLICATION='{}'
export VCAP_SERVICES='{"postgres": [{"credentials": {"username": "certpg", "host": "localhost", "password": "certpg", "name": "certpg", "port": 5434}, "tags": ["postgres"]}]}'
go run main.go
```

To checkout database:

```bash
psql "dbname=certpg host=localhost user=certpg password=certpg port=5434"
```
