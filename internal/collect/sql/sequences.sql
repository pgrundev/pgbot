-- Sequence bounds and direction. The effective range is intersected with the
-- owning column's integer range: an int4 column fed by a bigint sequence can
-- overflow before the sequence reaches its own bound or cycles.
-- last_value is null unless the role can read the sequence (SELECT/USAGE); those
-- rows are skipped, so a locked-down role degrades gracefully.
WITH ranges AS (
  SELECT s.schemaname   AS schema,
         s.sequencename AS sequence,
         s.last_value   AS last_value,
         greatest(s.min_value,
           CASE t.typname WHEN 'int2' THEN (-32768)::bigint
                          WHEN 'int4' THEN (-2147483648)::bigint
                          ELSE s.min_value END) AS floor,
         least(s.max_value,
           CASE t.typname WHEN 'int2' THEN 32767::bigint
                          WHEN 'int4' THEN 2147483647::bigint
                          ELSE s.max_value END) AS ceiling,
         s.min_value,
         s.max_value,
         s.increment_by AS increment,
         s.cycle,
         coalesce(reln.relname || '.' || att.attname, '') AS owned_by
  FROM pg_sequences s
  JOIN pg_class seqc      ON seqc.relname = s.sequencename
  JOIN pg_namespace seqn  ON seqn.oid = seqc.relnamespace AND seqn.nspname = s.schemaname
  LEFT JOIN pg_depend d   ON d.objid = seqc.oid AND d.classid = 'pg_class'::regclass
       AND d.refclassid = 'pg_class'::regclass AND d.deptype IN ('a', 'i')
  LEFT JOIN pg_class reln ON reln.oid = d.refobjid
  LEFT JOIN pg_attribute att ON att.attrelid = d.refobjid AND att.attnum = d.refobjsubid
  LEFT JOIN pg_type t     ON t.oid = att.atttypid
  WHERE s.last_value IS NOT NULL
)
SELECT schema,
       sequence,
       last_value,
       floor,
       ceiling,
       increment,
       cycle,
       floor <> min_value OR ceiling <> max_value AS column_limited,
       owned_by
FROM ranges
ORDER BY
  CASE WHEN NOT cycle OR floor <> min_value OR ceiling <> max_value THEN 0 ELSE 1 END,
  CASE
    WHEN floor >= ceiling OR last_value < floor OR last_value > ceiling THEN 1::numeric
    WHEN increment > 0 THEN
      (last_value::numeric - floor::numeric) /
        nullif(ceiling::numeric - floor::numeric, 0)
    ELSE
      (ceiling::numeric - last_value::numeric) /
        nullif(ceiling::numeric - floor::numeric, 0)
  END DESC,
  schema, sequence
LIMIT 50;
