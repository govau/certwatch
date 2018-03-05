-- The following same queries can be run against a CKAN API, e.g. put in a file "query.sql":
-- curl -G --data-urlencode sql@query.sql https://data.gov.au/api/3/action/datastore_search_sql
-- Pipe to jq to format in a table: ... | jq -r '.result.records[] | "\(.active_certificates)\t\(.issuer_cn)"'


-- Active certs by issuer
SELECT issuer_cn, COUNT(*) AS active_certificates
FROM "b718232a-bc8d-49c0-9c1f-33c31b57cd88"
WHERE not_valid_before < NOW() AND not_valid_after > NOW()
GROUP BY issuer_cn
ORDER BY active_certificates DESC;


-- Certificates for a given subdomain
SELECT r.issuer_cn, COUNT(*) AS active_certificates
FROM (
    SELECT s.key, s.issuer_cn AS issuer_cn
    FROM (
        SELECT key, UNNEST(domains) AS domain, issuer_cn
        FROM "b718232a-bc8d-49c0-9c1f-33c31b57cd88"
        WHERE not_valid_before < NOW() AND not_valid_after > NOW()
    ) s
    WHERE s.domain LIKE '%.tas.gov.au'
    GROUP BY s.key, s.issuer_cn
) r
GROUP BY r.issuer_cn
ORDER BY active_certificates DESC;
