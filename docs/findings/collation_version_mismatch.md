---
id: collation_version_mismatch
severity: warn
critical_when: "the database's default collation is the one that changed"
dimension: risk
object: db
scope: infra
requires: [PG15+]
thresholds: []
related: []
---

# collation_version_mismatch

**Severity:** warn (critical when the database's default collation is the one that changed) · **Dimension:** risk · **Object identity:** `db:<database>` (see [configuration](../configuration.md)) · **Requires:** PostgreSQL 15+

## What pgbot observed

The collation version the catalog recorded no longer matches what the server's
collation library reports now — for the database default
(`pg_database.datcollversion` vs `pg_database_collation_actual_version()`) or for
a named collation (`pg_collation.collversion` vs `pg_collation_actual_version()`).
Only this database is checked; collations whose provider records no version
(`C`, `POSIX`) cannot drift and are never reported.

Critical when the **database default** drifted, because every text index that
does not name a collation uses it. Warn when only named collations drifted.

## Why it matters

Text sort order is not defined by Postgres — it is defined by the OS's libc or
by ICU, and a btree index is only valid for the order that library produced when
the index was built. When the library changes underneath (an OS upgrade, a new
container base image, a restore onto a different host; glibc 2.28 changed the
order for most locales), the index is **silently out of order**: equality
lookups miss rows that exist, range scans skip them, and `UNIQUE` constraints
stop catching duplicates. Nothing errors. Postgres emits a `WARNING` at connect
time that nobody reads, and repairs nothing.

## How to verify it yourself

```sql
SELECT datname,
       datcollversion                              AS recorded,
       pg_database_collation_actual_version(oid)   AS actual
FROM pg_database
WHERE datname = current_database();
```

```sql
SELECT n.nspname || '.' || c.collname AS collation,
       c.collversion                     AS recorded,
       pg_collation_actual_version(c.oid) AS actual
FROM pg_collation c
JOIN pg_namespace n ON n.oid = c.collnamespace
WHERE c.collversion IS NOT NULL
  AND c.collversion IS DISTINCT FROM pg_collation_actual_version(c.oid);
```

A fresh `psql` session to the database prints the same thing Postgres sees:
`WARNING: database "app" has a collation version mismatch`.

## How to fix it

Rebuild first, then tell Postgres the new version is the right one. In that order.

1. **Reindex everything that sorts text with the affected collation.** For the
   database default that is every btree over a `text`/`varchar`/`char` column
   without an explicit `COLLATE`; the simple, safe answer is the whole database,
   online:

   ```sql
   REINDEX DATABASE CONCURRENTLY app;
   ```

   For a named collation, reindex the indexes whose columns use it. If you would
   rather check than rebuild, `amcheck` can verify btree order first
   (`CREATE EXTENSION amcheck; SELECT bt_index_check('index_name', true);`) — an
   index that passes is fine, one that fails must be rebuilt.

2. **Record the new version** so the warning stops and pgbot clears the finding:

   ```sql
   ALTER DATABASE app REFRESH COLLATION VERSION;
   -- or, for a named collation:
   ALTER COLLATION public.de_phonebook REFRESH VERSION;
   ```

Refreshing *before* reindexing only updates the catalog: the warning goes away
and the indexes stay corrupt. On a managed provider, check whether the provider
handled this as part of a major-version upgrade before doing it yourself; it is
still your indexes.

## When to ignore it

Only once the reindex is done and the refresh is scheduled, or when you have
verified with `amcheck` that every affected index is in order. A suppressed
critical still renders in the report; it only drops out of the exit code.

```toml
[[ignore]]
finding = "collation_version_mismatch"
object  = "db:app"
reason  = "reindexed after the glibc upgrade on 2026-09-10; REFRESH COLLATION VERSION in the next window"
expires = "2026-10-01"
```

## What pgbot cannot see

- Whether the sort order **actually changed** between the two versions for your
  locale — Postgres records versions, not orderings. A mismatch is a "may be
  corrupt", not a "is corrupt"; the only proof either way is `amcheck` or a
  rebuild.
- Which indexes use the affected collation. It reports the collation; mapping it
  to indexes is the reindex step.
- Other databases in the cluster. Each records its own `datcollversion`; run
  `--all-databases` to check them all.
- PostgreSQL 14 and older, which do not record a database-level version.

## Related

- [checksum_failures](checksum_failures.md) — the other silent-corruption signal,
  from the storage side rather than the collation library.
- [index_invalid](index_invalid.md) — an index Postgres already knows is unusable;
  a collation mismatch is one it still trusts.
