---
id: plan_cache_mode_forced
severity: info
critical_when: ""
dimension: latency
object: setting
scope: infra
requires: []
thresholds: []
related: [query_slowdown, stale_statistics]
---

# plan_cache_mode_forced

**Severity:** info · **Dimension:** latency · **Object identity:** `setting:plan_cache_mode` (see [configuration](../configuration.md)) · **Requires:** —

## What pgbot observed

`plan_cache_mode` is set to `force_generic_plan` or `force_custom_plan` at the
server, database or role level (pgbot reads the server's value, not its own
session's). The default is `auto`.

## Why it matters

For a prepared statement Postgres plans the first five executions with the actual
parameter values (custom plans), then switches to one generic plan only if its
estimated cost is not worse. That is what protects you from the
**parameter-sensitive plan** trap: a query whose best plan differs by argument
(one tenant with 40% of the rows, a status column where `'active'` is rare).
Forcing `force_generic_plan` cluster-wide bakes in the average-case plan for
every skewed argument; forcing `force_custom_plan` re-plans on every execution
and spends CPU on planning at high call rates. Both are legitimate for one
statement — rarely for a whole cluster.

## How to verify it yourself

```sql
SELECT name, setting, source, sourcefile, sourceline
FROM pg_settings
WHERE name = 'plan_cache_mode';
```

To see whether a specific prepared query is suffering, compare the generic plan
(`EXPLAIN (GENERIC_PLAN) SELECT … $1`, PG16+) with a plain `EXPLAIN` using a
skewed literal — no `ANALYZE` needed.

## How to fix it

```sql
ALTER SYSTEM RESET plan_cache_mode;           -- back to auto
SELECT pg_reload_conf();
-- then pin it where it was actually needed:
ALTER ROLE reporting SET plan_cache_mode = 'force_custom_plan';
```

Or `SET LOCAL plan_cache_mode = …` inside the transaction that runs the one
misbehaving statement (session-level `SET` breaks under transaction pooling).

## When to ignore it

A deliberate, documented choice — e.g. an ORM that prepares everything and a
workload with no skew, where `force_generic_plan` saves planning time.

```toml
[[ignore]]
finding = "plan_cache_mode_forced"
object  = "setting:plan_cache_mode"
reason  = "benchmarked: generic plans save 12% CPU, no skewed parameters"
expires = "2027-01-01"
```

## What pgbot cannot see

- Which statements are prepared, or their per-parameter plan choice —
  `pg_stat_statements` aggregates by query text, not by plan.
- Whether the setting was applied to work around a real regression.

## Related

- [query_slowdown](query_slowdown.md) — a prepared statement that got slower
  with unchanged SQL is the symptom this setting can cause or hide.
- [stale_statistics](stale_statistics.md) — the other reason a plan flips.
