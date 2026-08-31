-- transaction_contention_events is a bounded in-memory cluster event store.
-- Reading it performs one cluster RPC fanout, so aggregate once and cap the
-- returned hotspot list. Raw/pretty keys and transaction IDs are deliberately
-- excluded: both can disclose row-level values and are unnecessary for triage.
WITH grouped AS (
  SELECT encode(waiting_stmt_fingerprint_id, 'hex')                    AS waiting_stmt_fingerprint,
         encode(blocking_txn_fingerprint_id, 'hex')                    AS blocking_txn_fingerprint,
         coalesce(encode(blocking_stmt_fingerprint_id, 'hex'), '')     AS blocking_stmt_fingerprint,
         database_name,
         schema_name,
         table_name,
         coalesce(index_name, '')                                      AS index_name,
         contention_type,
         count(*)::int8                                                AS event_count,
         sum(extract(epoch FROM contention_duration))::float8          AS total_contention_seconds,
         max(extract(epoch FROM contention_duration))::float8          AS max_contention_seconds,
         max(collection_ts)                                            AS last_seen
  FROM crdb_internal.transaction_contention_events
  WHERE collection_ts >= now() - INTERVAL '1 hour'
  GROUP BY waiting_stmt_fingerprint_id, blocking_txn_fingerprint_id,
           blocking_stmt_fingerprint_id, database_name, schema_name,
           table_name, index_name, contention_type
), totals AS (
  SELECT *,
         sum(event_count) OVER ()::int8                                AS total_events,
         sum(total_contention_seconds) OVER ()::float8                 AS total_contention_seconds_all,
         max(max_contention_seconds) OVER ()::float8                   AS max_contention_seconds_all,
         sum(CASE WHEN contention_type = 'SERIALIZATION_CONFLICT'
                  THEN event_count ELSE 0 END) OVER ()::int8           AS serialization_conflicts
  FROM grouped
)
SELECT *
FROM totals
ORDER BY total_contention_seconds DESC, event_count DESC
LIMIT 25;
