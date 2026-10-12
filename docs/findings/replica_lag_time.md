---
id: replica_lag_time
severity: warn
critical_when: "replay lag past the critical threshold"
dimension: risk
object: cluster
scope: infra
requires: [replication, WAL flowing]
thresholds: [replica_lag_warn_seconds]
related: [sync_rep_degraded, recovery_conflicts]
---

# replica_lag_time

**Severity:** warn (critical when replay lag past the critical threshold) · **Dimension:** risk · **Object identity:** `cluster` (see [configuration](../configuration.md)) · **Requires:** replication, WAL flowing

## What pgbot observed

At least one standby's `replay_lag` in `pg_stat_replication` is **≥ 60 seconds**
(`warn`), or **≥ 300 seconds** (`critical`). The warn threshold is the
`replica_lag_warn_seconds` tunable (default `replicaLagWarnSec = 60`); the critical
threshold is fixed at `replicaLagCritSec = 300`. Raising the warning threshold
above 300 seconds does not suppress critical lag at or above 300 seconds. The
check is **gated on WAL
actually flowing** — pgbot only evaluates it when its own WAL sampling shows bytes
being written on the primary (`WAL.BytesPerSec > 0`). That gate matters:
`replay_lag` on an idle primary is stale and reads a reassuring "zero" even when a
standby is stuck, so a naive point-in-time read there lies. pgbot reports the
**worst** standby's lag as the headline number.

## Why it matters

Time-based replay lag *is* your recovery point. Promote this standby right now and you
lose every commit it hasn't replayed yet — that lag, in seconds of wall-clock work, is
exactly the data you'd drop on failover. Byte-based lag (`pg_wal_lsn_diff`) can't tell
you this: 200 MB of behind-ness is a catastrophe on a quiet database and a rounding
error on a busy one. Converting bytes to time needs the write rate, which is why
pgbot samples WAL generation. A standby that lags in replay is also a standby that
can't serve fresh reads, so read-your-writes breaks for anything routed to it.

## How to verify it yourself

Run on the **primary**. `replay_lag` is the time between a transaction committing
here and being replayed on the standby:

```sql
SELECT application_name,
       client_addr,
       state,
       EXTRACT(epoch FROM replay_lag)::int AS replay_lag_sec,   -- pgbot's number
       EXTRACT(epoch FROM write_lag)::int   AS write_lag_sec,
       EXTRACT(epoch FROM flush_lag)::int   AS flush_lag_sec
FROM   pg_stat_replication
ORDER  BY replay_lag DESC NULLS LAST;
```

Any standby with `replay_lag_sec ≥ 60` explains a `warn`; `≥ 300` explains a
`critical`. If every `replay_lag` is `NULL` or `0`, confirm the primary is actually
generating WAL right now (`SELECT pg_current_wal_lsn();` twice, a second apart) — pgbot
suppresses the finding on an idle primary for exactly this reason.

## How to fix it

Lag is a symptom; find where the standby's apply loop is stuck:

1. **Is the standby CPU- or IO-bound on replay?** Recovery is single-threaded. A
   standby on slower storage than the primary, or one also serving heavy read
   queries, can't apply WAL as fast as it arrives. Check its IO wait and the recovery
   process's CPU.
2. **Recovery conflicts pausing replay.** If `max_standby_streaming_delay` is high,
   the standby deliberately stalls WAL replay to let long read queries finish — see
   [recovery_conflicts](recovery_conflicts.md). Lower the delay, or move the long
   queries off the standby, to let replay keep up.
3. **The network between primary and standby.** Streaming can't outrun the link;
   check bandwidth and packet loss, and whether the standby fell back to restoring
   from the archive (much slower) after a disconnect.
4. **A slow consumer of the same slot** (for a logical/physical slot feeding this
   standby) can also stall it — check `pg_replication_slots`.

## When to ignore it

A read replica whose freshness genuinely doesn't matter — a reporting or analytics
standby you never fail over to and never read-your-writes against — can lag by design.
This is cluster-scoped, so the suppression silences lag for **every** standby, not
just the tolerant one; use it only when no standby's lag is operationally meaningful:

```toml
[[ignore]]
finding = "replica_lag_time"
reason  = "analytics replica is async-by-design and never promoted; lag is expected and unmonitored here"
expires = "2027-01-01"
```

If some standbys are failover targets and others aren't, prefer raising
`replica_lag_warn_seconds` to reduce subcritical warnings rather than muting the
finding outright. This remains a cluster-wide warning threshold, not a per-standby
filter: the fixed 300-second critical threshold still applies to every standby.

## What pgbot cannot see

- The lag is a **point-in-time** sample. A standby that lags in bursts (batch load,
  checkpoint) is only caught if the scan lands during a burst.
- It reports **replay** lag (the RPO-relevant one); it does not separately alert on
  `write_lag`/`flush_lag`, which matter for synchronous durability — see
  [sync_rep_degraded](sync_rep_degraded.md).
- The WAL-generation rate that gates this check is pgbot's own sample; a primary that
  was idle during the scan but busy otherwise can suppress a real lag.
- It cannot see *why* the standby is behind — only that it is. The apply-path
  investigation above is manual.

## Related

- [sync_rep_degraded](sync_rep_degraded.md) — a standby can be counted as synchronous
  yet still lag in replay; read the two together for the full failover picture.
- [recovery_conflicts](recovery_conflicts.md) — a common cause: replay deliberately
  paused to protect long-running read queries on the standby.
