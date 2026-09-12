package render

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/pgrundev/pgbot/internal/model"
)

// The gauge strip: four vital signs right under the header of the default
// view, each a filled bar, a value and a one-word status. It reads in a
// second and never contradicts the findings below it — every status word is
// driven by the finding that grades the same signal, never by a threshold of
// its own. A signal pgbot could not measure renders dim with "—" and says
// why, instead of showing an empty bar that looks like zero.
//
//	cache hit  [████████████████████]  99.2%     ok
//	lock wait  [████████████░░░░░░░░]  61.0%     query 4f2a
//	rollbacks  [██░░░░░░░░░░░░░░░░░░]  12.0%     watch
//	idle idx   [██████░░░░░░░░░░░░░░]  43.0 GiB  review
//
// Below it, the "checked" line names the subsystems that were collected and
// produced no finding — the compact form of the old GOOD list.

// gaugeWidth is the bar length in cells.
const gaugeWidth = 20

type gauge struct {
	label      string
	share      float64 // bar fill, 0..1
	value      string  // "99.4%", "43.0 GiB", or "—"
	status     string  // "ok", "low", "watch", "review", "query 4f2a", "2 blocked", or why it is not measurable
	kind       statusKind
	measurable bool
}

func notMeasurable(label, why string) gauge {
	return gauge{label: label, value: "—", status: why, kind: kInfo}
}

// gaugeCells rounds a share to the nearest cell, with a one-cell minimum for
// any non-zero value so a small but real signal never disappears.
func gaugeCells(share float64) int {
	if share <= 0 {
		return 0
	}
	if share > 1 {
		share = 1
	}
	n := int(share*gaugeWidth + 0.5)
	if n < 1 {
		n = 1
	}
	if n > gaugeWidth {
		n = gaugeWidth
	}
	return n
}

func gaugeBar(share float64) string {
	n := gaugeCells(share)
	return strings.Repeat("█", n) + strings.Repeat("░", gaugeWidth-n)
}

// firedIDs is the set of finding ids present on the context. Suppressed and
// preexisting findings count: a gauge must never read "ok" over a signal that
// produced a finding, however it is being displayed.
func firedIDs(c *model.Context) map[string]bool {
	m := make(map[string]bool, len(c.Findings))
	for _, f := range c.Findings {
		m[f.ID] = true
	}
	return m
}

// cacheHitGauge: the sampled cache-hit ratio. Graded by the low_cache_hit
// finding; a thin sample (under CacheHitMinBlocks) is not graded at all.
func cacheHitGauge(c *model.Context) gauge {
	h := c.Health
	if h == nil || h.CacheHitRatio == nil {
		return notMeasurable("cache hit", "not measurable")
	}
	if !h.CacheHitUsable() {
		return notMeasurable("cache hit", "thin sample")
	}
	g := gauge{label: "cache hit", share: *h.CacheHitRatio, value: pct(*h.CacheHitRatio), status: "ok", kind: kOK, measurable: true}
	if firedIDs(c)["low_cache_hit"] {
		g.status, g.kind = "low", kBad
	}
	return g
}

// lockWaitGauge: the Lock bucket's share of sampled active time, from the wait
// profile. With blocked sessions the status names the culprit — the query
// with the highest lock share — or falls back to the blocked count when
// attribution is missing. Without a wait profile only the status is shown.
func lockWaitGauge(c *model.Context) gauge {
	wp := c.WaitProfile
	profiled := wp != nil && wp.Available
	if !profiled && c.Locks == nil {
		return notMeasurable("lock wait", "not measurable")
	}
	g := gauge{label: "lock wait", value: "—", status: "—", kind: kInfo, measurable: true}
	if profiled {
		for _, b := range wp.Buckets {
			if b.Type == "Lock" {
				g.share = b.Share
				break
			}
		}
		g.value = pct(g.share)
	}
	if c.Locks == nil {
		return g
	}
	blocked := c.Locks.BlockedCount
	if blocked == 0 {
		g.status, g.kind = "ok", kOK
		return g
	}
	g.kind = kBad
	g.status = fmt.Sprintf("%d blocked", blocked)
	if profiled {
		best := -1.0
		var culprit int64
		for _, q := range wp.ByQuery {
			if q.LockShare > best && q.LockShare > 0 {
				best, culprit = q.LockShare, q.QueryID
			}
		}
		if best > 0 {
			g.status = "query " + queryHex4(culprit)
		}
	}
	return g
}

// queryHex4 is the first four hex digits of a query_id — enough to find it in
// `pgbot queries` / pg_stat_statements without eating the row.
func queryHex4(id int64) string {
	return fmt.Sprintf("%016x", uint64(id))[:4]
}

// rollbacksGauge: the rollback ratio over the sample window, graded by the
// high_rollback_ratio finding.
func rollbacksGauge(c *model.Context) gauge {
	h := c.Health
	if h == nil || h.RollbackRatio == nil {
		return notMeasurable("rollbacks", "not measurable")
	}
	g := gauge{label: "rollbacks", share: *h.RollbackRatio, value: pct(*h.RollbackRatio), status: "ok", kind: kOK, measurable: true}
	if firedIDs(c)["high_rollback_ratio"] {
		g.status, g.kind = "watch", kWatch
	}
	return g
}

// idleIndexGauge: the summed size of zero-scan indexes; the bar is that size
// as a share of the database, so it answers "how much of my storage is dead
// weight". Graded by the unused_indexes finding. Meaningless in a cold stats
// window, exactly like the finding.
func idleIndexGauge(c *model.Context) gauge {
	if c.Window.ColdWindow() {
		return notMeasurable("idle idx", "window < 15m")
	}
	if c.Indexes == nil {
		return notMeasurable("idle idx", "not measurable")
	}
	var idle int64
	for _, ix := range c.Indexes.Unused {
		if ix.Scans == 0 {
			idle += ix.Bytes
		}
	}
	g := gauge{label: "idle idx", value: humanBytes(idle), status: "ok", kind: kOK, measurable: true}
	if c.Tables != nil && c.Tables.DBSizeBytes > 0 {
		g.share = float64(idle) / float64(c.Tables.DBSizeBytes)
	}
	if firedIDs(c)["unused_indexes"] {
		g.status, g.kind = "review", kWatch
	}
	return g
}

// renderGauges prints the strip. Layout is fixed-width so it fits the
// 80-column floor with the longest status: two spaces, a 9-wide label, the
// bracketed bar, an 8-wide value, then the status.
func renderGauges(b *strings.Builder, st styler, c *model.Context, _ int) {
	for _, g := range []gauge{cacheHitGauge(c), lockWaitGauge(c), rollbacksGauge(c), idleIndexGauge(c)} {
		paint := statusColor(st, g.kind)
		if !g.measurable {
			paint = st.dim
		}
		label := fmt.Sprintf("%-9s", g.label)
		value := fmt.Sprintf("%-8s", g.value)
		fmt.Fprintf(b, "  %s  [%s]  %s  %s\n", st.dim(label), paint(gaugeBar(g.share)), value, paint(g.status))
	}
	fmt.Fprintln(b)
}

// checkedSubsystem is a subsystem the "checked" line may name: it counts as
// checked-clean when it was collected and none of its findings fired.
type checkedSubsystem struct {
	name      string
	collected bool
	findings  []string
}

// buildChecked names, in a fixed order, the subsystems pgbot collected and
// found nothing to say about. Only collected subsystems are ever named — a
// section pgbot did not gather cannot be "checked".
func buildChecked(c *model.Context) []string {
	fired := firedIDs(c)
	deadlocksClean := c.Health != nil && c.Health.DeadlocksPerMin != nil && *c.Health.DeadlocksPerMin == 0
	subsystems := []checkedSubsystem{
		{"queries", c.Queries != nil && c.Queries.Enabled,
			[]string{"query_slowdown", "seq_scan_heavy", "partition_seq_scan_heavy", "pgss_entries_evicted", "pg_stat_statements_missing"}},
		{"indexes", c.Indexes != nil,
			[]string{"unused_indexes", "index_invalid", "redundant_indexes", "fk_unindexed"}},
		{"vacuum", c.Tables != nil,
			[]string{"table_bloat", "autovacuum_disabled_on_table", "table_never_vacuumed", "autovacuum_starved", "autovacuum_saturated",
				"autovacuum_long_running", "stale_statistics", "never_analyzed", "low_hot_update_ratio", "vacuum_horizon_blocked", "autovacuum_off"}},
		{"replication", c.Replication != nil,
			[]string{"sync_rep_degraded", "replica_lag_time", "recovery_conflicts", "replica_disconnected", "replication_slot_inactive", "subscription_worker_down"}},
		{"checkpoints", c.WAL != nil, []string{"checkpoints_forced"}},
		{"io", c.IOStats != nil && c.IOStats.Exactness == model.ExactnessSampled && c.IOStats.TrackIOTiming, []string{"io_read_latency_high"}},
		{"connections", c.Limits != nil && c.Limits.ConnectionsMax > 0,
			[]string{"connection_saturation", "connections_overprovisioned", "idle_in_transaction", "long_running_transaction", "prepared_xact_abandoned"}},
		{"settings", c.Settings != nil,
			[]string{"work_mem_low", "fsync_off", "full_page_writes_off", "autovacuum_off", "random_page_cost_high", "work_mem_overcommit",
				"statement_timeout_unset", "io_timing_off", "checksums_disabled", "ignore_checksum_failure_on",
				"io_concurrency_low", "plan_cache_mode_forced", "slot_wal_keep_unbounded"}},
		{"wraparound", c.Limits != nil && c.Limits.MaxXIDAge > 0, []string{"txid_wraparound", "mxid_wraparound", "sequence_exhaustion"}},
		{"deadlocks", deadlocksClean, []string{"blocking_chains"}},
	}
	// A subsystem whose gauge is already orange or red stays off the line, so
	// the line never contradicts the strip.
	if g := idleIndexGauge(c); g.measurable && g.kind != kOK {
		fired["unused_indexes"] = true
	}
	var out []string
	for _, s := range subsystems {
		if !s.collected {
			continue
		}
		clean := true
		for _, id := range s.findings {
			if fired[id] {
				clean = false
				break
			}
		}
		if clean {
			out = append(out, s.name)
		}
	}
	return out
}

// renderChecked prints "checked · a · b · c", wrapping whole items with an
// eight-space hanging indent so continuation lines align under the first item.
func renderChecked(b *strings.Builder, st styler, items []string, width int) {
	if len(items) == 0 {
		return
	}
	const indent = "        "
	var lines []string
	cur := "checked"
	for _, it := range items {
		piece := " · " + it
		if utf8.RuneCountInString(cur+piece) > width && cur != "checked" && cur != indent+"·" {
			lines = append(lines, cur)
			cur = indent + "·" + " " + it
			continue
		}
		cur += piece
	}
	lines = append(lines, cur)
	for i, l := range lines {
		if i == 0 {
			fmt.Fprintf(b, "%s%s\n", st.good("checked"), st.dim(strings.TrimPrefix(l, "checked")))
		} else {
			fmt.Fprintln(b, st.dim(l))
		}
	}
	fmt.Fprintln(b)
}
