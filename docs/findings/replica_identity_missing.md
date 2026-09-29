---
id: replica_identity_missing
severity: critical
critical_when: ""
dimension: risk
object: relation
scope: schema
requires: [a publication replicating UPDATE or DELETE]
thresholds: []
related: [subscription_worker_down, replication_slot_inactive]
---

# replica_identity_missing

**Severity:** critical · **Dimension:** risk · **Object identity:** `schema.table` (see [configuration](../configuration.md)) · **Requires:** a publication that replicates `UPDATE` or `DELETE`

## What pgbot observed

A table belongs to a publication that replicates `UPDATE` or `DELETE`, but its
replica identity cannot identify a row:

- `DEFAULT` with no primary key — the default resolves to the primary key, and
  there isn't one.
- `NOTHING` — set explicitly.
- `USING INDEX` whose nominated index is missing or invalid, which behaves like
  `NOTHING`.

Publications that replicate only `INSERT` need no identity and are not reported.
Everything here comes from `pg_publication` and `pg_class`, so it is valid on a
freshly migrated, never-queried database.

## Why it matters

Postgres accepts the table, the publication and the schema, then refuses the
write at runtime:

```
ERROR: cannot update table "events" because it does not have a replica identity
       and publishes updates
HINT:  To enable updating the table, set REPLICA IDENTITY using ALTER TABLE.
```

`SELECT` and `INSERT` keep working, so nothing fails at deploy time. The failure
arrives with the first `UPDATE` or `DELETE` after the migration, in whatever code
path happens to run it, and it is a hard error for that statement rather than a
replication lag or a warning.

## How to verify it yourself

```sql
SELECT n.nspname AS schema,
       c.relname AS "table",
       c.relreplident AS identity,
       string_agg(DISTINCT p.pubname, ', ') AS publications
FROM pg_publication p
JOIN pg_publication_tables pt ON pt.pubname = p.pubname
JOIN pg_namespace n ON n.nspname = pt.schemaname
JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = pt.tablename
WHERE (p.pubupdate OR p.pubdelete)
  AND c.relkind IN ('r', 'p')
  AND (
    (c.relreplident = 'd' AND NOT EXISTS (
       SELECT 1 FROM pg_index i WHERE i.indrelid = c.oid AND i.indisprimary AND i.indisvalid))
    OR c.relreplident = 'n'
    OR (c.relreplident = 'i' AND NOT EXISTS (
       SELECT 1 FROM pg_index i WHERE i.indrelid = c.oid AND i.indisreplident AND i.indisvalid))
  )
GROUP BY 1, 2, 3
ORDER BY 1, 2;
```

## How to fix it

Pick the cheapest identity the table can support.

1. **A primary key**, if the table can have one. This is the normal answer and
   needs no further configuration:

   ```sql
   ALTER TABLE public.events ADD PRIMARY KEY (id);
   ```

2. **An existing unique index** on `NOT NULL` columns, when a primary key is not
   an option:

   ```sql
   ALTER TABLE public.events REPLICA IDENTITY USING INDEX events_uniq_idx;
   ```

3. **`FULL`**, when neither fits. Correct, but it writes every column into the WAL
   record and makes the subscriber match rows without an index:

   ```sql
   ALTER TABLE public.events REPLICA IDENTITY FULL;
   ```

4. **Remove the table from the publication**, if it was never meant to replicate:

   ```sql
   ALTER PUBLICATION app_pub DROP TABLE public.events;
   ```

## When to ignore it

When the table is genuinely insert-only and you are certain no `UPDATE` or
`DELETE` will ever run against it. That is a claim about application behaviour
that the catalog cannot confirm, and one stray `UPDATE` turns into an error, so
scope the suppression to the table and give it an expiry:

```toml
[[ignore]]
finding = "replica_identity_missing"
object  = "public.events"
reason  = "append-only event log; no UPDATE/DELETE in the writer (OPS-2291)"
expires = "2027-01-01"
```

## What pgbot cannot see

- Whether anything actually issues an `UPDATE` or `DELETE` against the table. The
  finding reports that the write *would* fail, not that it has.
- The subscriber side. A subscription may also need its own matching index for
  `FULL` identity to perform acceptably.
- Row filters and column lists on the publication, which narrow what replicates
  but do not remove the identity requirement.
- A partitioned table published with `publish_via_partition_root` replicates as
  the root; pgbot names the relation the catalog lists, which may be a partition.

## Related

- [subscription_worker_down](subscription_worker_down.md) — the subscriber-side
  symptom when replication stops.
- [replication_slot_inactive](replication_slot_inactive.md) — a stalled logical
  slot retains WAL while the publisher cannot make progress.
