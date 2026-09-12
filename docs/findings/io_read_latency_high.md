---
id: io_read_latency_high
severity: warn
critical_when: "mean physical read latency ≥ 20 ms"
dimension: latency
object: cluster
scope: workload
requires: [PG16+, track_io_timing]
thresholds: []
related: [wait_io_bound, low_cache_hit, seq_scan_heavy, io_concurrency_low]
---

# io_read_latency_high

**Severity:** warn (critical at ≥ 20 ms) · **Dimension:** latency · **Object identity:** `cluster` (see [configuration](../configuration.md)) · **Requires:** PostgreSQL 16+ (`pg_stat_io`), `track_io_timing = on`

## What pgbot observed

Over the sample window, `pg_stat_io` recorded at least **500** physical reads
(blocks that missed `shared_buffers`) and their mean latency —
`Δread_time / Δreads` across every backend type, object and context — was
**≥ 5 ms** (`ioReadLatencyWarnMS`). At **≥ 20 ms** (`ioReadLatencyCritMS`) the
finding is critical. The 500-read floor (`ioReadLatencyMinOps`) keeps a handful of
cold reads from producing a meaningless mean. The evidence names the heaviest
reading backend/context and, when the window carries enough block traffic, the
shared-buffer hit ratio.

## Why it matters

A read that misses `shared_buffers` is not necessarily slow: if the kernel page
cache has the block, it returns in microseconds. A mean in the milliseconds means
the block was fetched from the device — or from a network volume with a latency
floor — on every miss. That is the definition of a storage-bound workload: each
scan step, each index descent into a cold page, pays that price, and it shows up
as `IO`-class waits and a flat CPU graph. Raising `shared_buffers` only helps if
the working set then fits; reading fewer blocks always helps.

## How to verify it yourself

```sql
-- Mean latency per physical read since the last stats reset, by backend/context.
-- Needs track_io_timing = on; otherwise read_time is 0 and the mean is meaningless.
SELECT backend_type, object, context,
       reads,
       round((read_time / nullif(reads, 0))::numeric, 2) AS mean_read_ms
FROM pg_stat_io
WHERE reads > 0
ORDER BY reads DESC;
```

For a window rather than a cumulative average, run it twice a minute apart and
divide the deltas.

## How to fix it

In this order — each step is cheaper and more durable than the next:

1. **Read fewer blocks.** `pgbot queries` ranks statements by total time; the ones
   with high `shared_blks_read` per call are the targets. An index that turns a
   scan into a lookup ([seq_scan_heavy](seq_scan_heavy.md)), a `LIMIT`, or a
   narrower row removes reads outright. Check
   [table_bloat](table_bloat.md) — dead space inflates the block count of every scan.
2. **Fit the working set.** On a dedicated host `shared_buffers` ≈ 25% of RAM is the
   documented starting point; going past ~40% rarely helps because the OS cache does
   the rest. Confirm with the hit ratio after the change, not before.
3. **Then the IO knobs.** `effective_io_concurrency` in steps (16 → 32), and on
   PostgreSQL 18 `io_method` (`worker`, or `io_uring` where the build has it) —
   re-measure this latency after each step. Past the device's useful queue depth,
   more concurrency raises latency for every session.
4. **Then the volume.** On managed providers a higher IOPS/throughput class
   (io2 / pd-ssd → faster tier) is the last lever.

## When to ignore it

- The window caught a one-off cold start (restart, failover) — re-run after warmup.
- An analytical/batch workload that legitimately streams from disk and meets its
  latency target anyway.

```toml
[[ignore]]
finding = "io_read_latency_high"
reason  = "nightly ETL window; reads are sequential and within SLO"
expires = "2027-01-01"
```

## What pgbot cannot see

- Whether a miss was served by the kernel page cache or the device — only the
  latency distinguishes them, which is why the finding needs `track_io_timing`.
- The device's queue depth or the cloud volume's provisioned IOPS; correlate with
  host or provider telemetry before buying a faster volume.
- A mean hides a bimodal distribution (most reads fast, a few very slow).

## Related

- [wait_io_bound](wait_io_bound.md) — the same problem seen from sampled waits.
- [low_cache_hit](low_cache_hit.md) — the ratio side; this finding is the cost side.
- [seq_scan_heavy](seq_scan_heavy.md) — the usual reason so many blocks are read.
- [io_concurrency_low](io_concurrency_low.md) — the knob that lets scans overlap reads.
