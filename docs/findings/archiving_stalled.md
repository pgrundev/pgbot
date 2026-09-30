---
id: archiving_stalled
severity: critical
critical_when: "downgraded to info on a managed provider"
dimension: risk
object: cluster
scope: infra
requires: [primary, archive_mode=on]
thresholds: []
related: [archiving_failing]
---

# archiving_stalled

**Severity:** critical · **Dimension:** risk · **Object identity:** `cluster` (see [configuration](../configuration.md)) · **Requires:** primary, archive_mode=on

## What pgbot observed

`archive_mode` is `on` (or `always`), pgbot's current WAL sample has
`BytesPerSec > 0`, and `Context.CollectedAt - last_archived_time` exceeds the stall
threshold, which is
**`max(archive_timeout × 3, 1 hour)`** (`archiveStallFloorS = 3600`). Crucially this is
the case where archiving is **not reporting a failure** — `last_failed_time` is not
newer than `last_archived_time`, so [archiving_failing](archiving_failing.md) doesn't
fire. The elapsed time is measured at collection, not when a stored Context is viewed,
so recomputing the same Context produces the same finding. A zero `CollectedAt` is
unknown and does not fall back to the viewer's clock. As with all archiving checks,
pgbot reads only `pg_stat_archiver` timestamps and never the `archive_command` value.
On a detected managed provider this is downgraded to `info`.

These signals do not prove WAL was generated continuously since the last archive. The
WAL rate is a short sample at collection time, and this finding is emitted only when a
previous successful archive timestamp exists. pgbot does not substitute
`pg_stat_archiver.stats_reset` when no archive has ever succeeded because that reset
time says nothing about when `archive_mode` became effective or when an archivable WAL
segment became eligible.

## Why it matters

If the signals persist while completed WAL segments become eligible, a silent stall is
more dangerous than a clean failure because there may be no error to trip on. PostgreSQL
retains WAL that still needs archiving, so a confirmed archiver stall can widen the gap
in archive coverage and grow `pg_wal` toward a disk-full outage. Common causes include a
hung `archive_command`, a stopped archiver process, or a destination that accepts a
connection but never completes the write. The single pgbot sample is a prompt to verify
that progress is actually stuck, not proof of how long WAL generation or archive
failure has continued.

## How to verify it yourself

Run on the **primary**. This reproduces the stall test — the age of the last archive
against `max(archive_timeout × 3, 1h)`:

```sql
SELECT last_archived_time,
       now() - last_archived_time AS since_last_archive,
       last_failed_time,
       (SELECT setting::int FROM pg_settings WHERE name = 'archive_timeout')  AS archive_timeout_s,
       (SELECT setting      FROM pg_settings WHERE name = 'archive_mode')     AS archive_mode,
       GREATEST(3 * (SELECT setting::int FROM pg_settings WHERE name = 'archive_timeout'),
                3600) * interval '1 second'                                   AS stall_threshold
FROM   pg_stat_archiver;
```

If `since_last_archive > stall_threshold`, `archive_mode` is on, `last_failed_time` is
**not** newer than `last_archived_time`, and the primary is writing WAL right now
(`SELECT pg_current_wal_lsn();` twice, a second apart, to confirm), the query reproduces
the signals pgbot observed. Confirm that a completed segment is eligible and that
`last_archived_time` still does not advance before diagnosing a stall. Check `pg_wal`
size too: `SELECT pg_size_pretty(sum(size)) FROM pg_ls_waldir();`.

## How to fix it

After confirming eligible WAL is not progressing, check *liveness* before configuration:

1. **Is the archiver process alive and moving?** Look for the archiver in
   `pg_stat_activity` / the process list. If `archive_command` hangs (a blocking NFS
   mount, a TCP connection with no timeout), the archiver waits forever and archives
   nothing; kill the stuck command and give the target a hard timeout.
2. **Is the destination reachable but not completing writes?** A store that accepts
   connections but stalls on write (throttling, a full-but-silent bucket, a wedged
   gateway) produces exactly this — no failure, no progress. Test a manual write to the
   target.
3. **Force a segment switch to confirm recovery**: `SELECT pg_switch_wal();` then watch
   `last_archived_time` advance in the query above. Once it moves, the retained WAL
   drains and `pg_wal` recycles back down.
4. **Relieve the disk if it's near full** while you fix the archiver — but never delete
   files from `pg_wal` by hand; let archiving catch up so Postgres recycles them safely.

**On a managed provider this is the provider's responsibility.** RDS/Aurora, Cloud SQL,
Azure, Supabase and Neon ship WAL and take backups outside `archive_command`, so pgbot
downgrades this to `info` — verify backup status in the provider console rather than
touching archiving yourself.

## When to ignore it

Do not suppress the finding merely because the last archive is old. First distinguish a
persistent stall from a newly enabled archiver, WAL activity that just resumed after a
long idle period, or a sample taken before a segment became eligible. If a persistent
stall is already a known, tracked incident under active remediation, a temporary
suppression can keep it out of the exit code while leaving it visible. Cluster-scoped:

```toml
[[ignore]]
finding = "archiving_stalled"
reason  = "archive target throttling under investigation 2026-08-17; pg_wal monitored, tracked in OPS-1234"
expires = "2026-09-01"
```

## What pgbot cannot see

- It reads `last_archived_time`, not the archiver's internals — it can't tell a **hung**
  `archive_command` from a **dead** archiver process; both look like "no recent
  archive." The process check above is manual.
- It **never reads the `archive_command` string** (a credentials/secret leak vector),
  so it can't point at a misconfigured command — only at the resulting silence.
- The stall gate depends on pgbot's **point-in-time** WAL sample; a primary that was
  briefly idle during the scan can suppress a real stall. Conversely, activity that
  resumed just before the sample does not prove WAL flowed throughout the time since the
  last archive or that a completed segment has already become eligible.
- With no prior successful archive, the collected fields cannot distinguish a broken
  archiver from newly enabled archiving or a workload that has not completed a segment.
  `stats_reset` is deliberately not treated as the start of a failure interval.
- On a managed provider it cannot see the provider's own WAL-shipping, which may be
  healthy while `pg_stat_archiver` looks stalled — hence the downgrade.

## Related

- [archiving_failing](archiving_failing.md) — the sibling case where the archiver *is*
  reporting failures; a stall is specifically the no-error, no-progress variant this
  finding covers.
