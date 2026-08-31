-- Before system.statements, normalized query text lived in the SQL-statistics
-- metadata JSON. Keep the same bounded fingerprint lookups for those versions.
WITH direct_statements AS (
  SELECT ss.fingerprint_id,
         array_agg(DISTINCT ss.app_name)                    AS app_names,
         max(coalesce(ss.metadata->>'query', ''))           AS query
  FROM system.statement_statistics@fingerprint_stats_idx AS ss
  WHERE ss.fingerprint_id IN (%s)
    AND ss.aggregated_ts >= now() - INTERVAL '24 hours'
  GROUP BY ss.fingerprint_id
), raw_transaction_statements AS (
  SELECT DISTINCT ts.fingerprint_id AS transaction_fingerprint_id,
                  ts.app_name,
                  jsonb_array_elements_text(ts.metadata->'stmtFingerprintIDs') AS statement_fingerprint
  FROM system.transaction_statistics@fingerprint_stats_idx AS ts
  WHERE ts.fingerprint_id IN (%s)
    AND ts.aggregated_ts >= now() - INTERVAL '24 hours'
), transaction_statements AS (
  SELECT transaction_fingerprint_id,
         statement_fingerprint,
         array_agg(DISTINCT app_name) AS app_names
  FROM raw_transaction_statements
  GROUP BY transaction_fingerprint_id, statement_fingerprint
), ranked_transaction_statements AS (
  SELECT *, row_number() OVER (
    PARTITION BY transaction_fingerprint_id ORDER BY statement_fingerprint
  ) AS statement_rank
  FROM transaction_statements
), blocker_statements AS (
  SELECT tx.transaction_fingerprint_id,
         tx.statement_fingerprint,
         tx.app_names,
         coalesce(max(ss.metadata->>'query'), '') AS query
  FROM ranked_transaction_statements AS tx
  LEFT JOIN system.statement_statistics@fingerprint_stats_idx AS ss
    ON ss.fingerprint_id = decode(tx.statement_fingerprint, 'hex')
   AND ss.aggregated_ts >= now() - INTERVAL '24 hours'
  WHERE tx.statement_rank <= 5
  GROUP BY tx.transaction_fingerprint_id, tx.statement_fingerprint, tx.app_names
)
SELECT 'statement'                                AS attribution_kind,
       encode(fingerprint_id, 'hex')              AS fingerprint,
       ''                                         AS transaction_fingerprint,
       coalesce(query, '')                        AS query,
       app_names
FROM direct_statements
UNION ALL
SELECT 'transaction'                              AS attribution_kind,
       statement_fingerprint                      AS fingerprint,
       encode(transaction_fingerprint_id, 'hex')  AS transaction_fingerprint,
       query,
       app_names
FROM blocker_statements;
