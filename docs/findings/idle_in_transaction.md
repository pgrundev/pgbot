---
id: idle_in_transaction
severity: warn
critical_when: "escalates with the number and age of idle-in-transaction sessions"
dimension: risk
object: cluster
scope: workload
requires: []
thresholds: []
related: [long_running_transaction, vacuum_horizon_blocked]
---

# idle_in_transaction

**Severity:** warn (critical when escalates with the number and age of idle-in-transaction sessions) · **Dimension:** risk · **Object identity:** `cluster` (see [configuration](../configuration.md)) · **Requires:** —

## What pgbot observed

At least one session was in state **`idle in transaction`** when pgbot sampled
`pg_stat_activity` (`c.Activity.IdleInTransaction > 0`) — it has an open
transaction but is running no statement, waiting on the client. The finding starts
at `info`; it **escalates to `warn` once the oldest idle-in-transaction session reaches
`idleInTxnWarnSec` = 60 seconds**. The impact score grows with
the age of the oldest idle transaction: `min(80, 50 + longestIdleXactSec÷30)` in the escalated case
(baseline `30`). Ages from active sessions do not escalate this finding;
`LongestXactSec` still describes the oldest transaction of any state and drives
`long_running_transaction` separately. Older stored snapshots without an
idle-specific age retain the baseline `info` finding and report that the idle
transaction age was not measured.

## Why it matters

An idle-in-transaction session is doing nothing but is the opposite of harmless. It
still **holds every lock it has taken**, so it can block DDL, migrations, and other
writers (see [blocking_chains](blocking_chains.md)). Worse, it **pins the `xmin`
horizon**: while its transaction is open, `VACUUM` cannot reclaim any row version
that became dead after it started, anywhere in the cluster. Dead tuples accumulate,
tables and indexes bloat, and on a long enough leash transaction-id age climbs
toward wraparound. The usual cause is an application bug — a `BEGIN` with no timely
`COMMIT`/`ROLLBACK`, a transaction left open across a slow external call, or a
connection returned to a pool without being reset.

## How to verify it yourself

```sql
SELECT pid, usename, application_name, client_addr,
       state,
       now() - xact_start    AS xact_age,
       now() - state_change  AS idle_for,
       left(query, 60)       AS last_statement
FROM pg_stat_activity
WHERE state IN ('idle in transaction', 'idle in transaction (aborted)')
ORDER BY xact_start;
```

The row count is pgbot's `IdleInTransaction`; the largest `xact_age` is the
`LongestIdleXactSec` that drives the escalation to `warn`. `idle in transaction
(aborted)` is the same hazard after an error left the transaction unrollbacked — it
still holds its snapshot.

## How to fix it

1. **End the stuck sessions.** For a session running no statement, `pg_cancel_backend`
   does nothing — you must terminate the whole backend:
   ```sql
   SELECT pg_terminate_backend(pid)
   FROM pg_stat_activity
   WHERE state IN ('idle in transaction', 'idle in transaction (aborted)')
     AND now() - state_change > interval '5 minutes';
   ```
2. **Stop it recurring at the server.** Set a bound so no session can hold a
   transaction open idle indefinitely — Postgres will terminate it automatically:
   ```sql
   ALTER SYSTEM SET idle_in_transaction_session_timeout = '60s';
   SELECT pg_reload_conf();
   ```
   (Set it per-role or per-database if a specific workload legitimately needs longer.)
3. **Fix the application.** Shorten transaction scope, never hold a transaction open
   across a network round-trip or user think-time, and make sure your pool issues a
   `ROLLBACK`/`DISCARD ALL` when returning a connection.

## When to ignore it

A known batch job or admin session that deliberately holds a transaction briefly,
and you accept the transient lock/vacuum cost. Because the finding identifies
sessions by **PID** — ephemeral identifiers with no stable name — there is nothing
to scope an ignore to; suppression is cluster-wide by design:

```toml
[[ignore]]
finding = "idle_in_transaction"
reason  = "nightly ETL parks a short idle-in-transaction window; accepted, tracked in OPS-2201"
expires = "2027-01-01"
```

## What pgbot cannot see

- It sees a **snapshot**, not history. A session that flickers into and out of
  `idle in transaction` between samples may be undercounted, and one it reports may
  already have committed.
- It cannot see **which application code** opened the transaction or why the client
  went quiet — only the state, age, and PID.
- Sessions are keyed by **PID**, an ephemeral identifier that is gone by the next
  run. That is why this finding is `cluster`-scoped and its `[[ignore]]` has no
  `object` line: there is no durable object to suppress, so it is all or nothing —
  deliberately, so a genuinely stuck transaction tomorrow still surfaces.

## Related

- [long_running_transaction](long_running_transaction.md) — the same xmin-pinning
  hazard, but from a transaction that is *busy* rather than idle.
- [vacuum_horizon_blocked](vacuum_horizon_blocked.md) — when an idle-in-transaction
  session is the oldest xmin holder, it is what freezes the vacuum horizon.
