-- Partitioned-table rollup. pg_stat_user_tables lists every leaf partition as its
-- own relation, so a 200-partition table floods the top-N and each partition's
-- seq_scan looks harmless while the PARENT is scanned end to end. Climb pg_inherits
-- from each leaf partition to its root (topmost non-partition ancestor) and
-- aggregate size + scan counts + partition count. Per-partition detail stays in
-- the tables section; this is the parent-level view seq_scan_heavy needs.
WITH RECURSIVE climb AS (
  SELECT c.oid AS leaf, c.oid AS node, c.relispartition
  FROM pg_class c
  WHERE c.relkind = 'r' AND c.relispartition
  UNION ALL
  SELECT climb.leaf, i.inhparent, pc.relispartition
  FROM climb
  JOIN pg_inherits i ON i.inhrelid = climb.node
  JOIN pg_class pc   ON pc.oid = i.inhparent
  WHERE climb.relispartition
),
roots AS (
  SELECT leaf, node AS root FROM climb WHERE NOT relispartition
),
leaves AS (
  SELECT r.root, s.relid, s.relname, s.n_live_tup,
         s.seq_scan + coalesce(s.idx_scan, 0) AS scans,
         pg_total_relation_size(s.relid)      AS bytes
  FROM roots r
  JOIN pg_stat_user_tables s ON s.relid = r.leaf
),
-- The hottest leaf by scans and the largest leaf by rows: the skew evidence.
-- Cumulative counters, so a freshly attached partition looks cold (A-skew).
hot AS (
  SELECT DISTINCT ON (root) root, relname AS hot_partition, scans AS hot_scans
  FROM leaves ORDER BY root, scans DESC, relname
),
big AS (
  SELECT DISTINCT ON (root) root, relname AS big_partition, n_live_tup AS big_rows
  FROM leaves ORDER BY root, n_live_tup DESC, relname
)
SELECT n.nspname                 AS schema,
       rc.relname                AS "table",
       count(*)                  AS partitions,
       sum(l.bytes)              AS total_bytes,
       sum(l.n_live_tup)         AS live_tuples,
       sum(l.scans) - sum(coalesce(s.idx_scan, 0)) AS seq_scans,
       sum(coalesce(s.idx_scan, 0)) AS index_scans,
       max(hot.hot_partition)    AS hot_partition,
       max(hot.hot_scans)        AS hot_scans,
       max(big.big_partition)    AS big_partition,
       max(big.big_rows)         AS big_rows
FROM leaves l
JOIN pg_stat_user_tables s ON s.relid = l.relid
JOIN pg_class rc     ON rc.oid = l.root
JOIN pg_namespace n  ON n.oid = rc.relnamespace
JOIN hot ON hot.root = l.root
JOIN big ON big.root = l.root
GROUP BY 1, 2
ORDER BY total_bytes DESC
LIMIT 20;
