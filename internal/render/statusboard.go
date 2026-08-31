package render

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/pgrundev/pgbot/internal/model"
)

// The --full view leads with a subsystem status board: one box-drawn row per
// subsystem with a status word, a headline value, and a short note. It's the
// at-a-glance overview; the detailed section tables follow below it.

type boardRow struct {
	subsystem string
	status    string // ok | warn | fail
	kind      statusKind
	value     string
	note      string
}

func renderBoard(b *strings.Builder, st styler, rows []boardRow) {
	if len(rows) == 0 {
		return
	}
	head := []string{"subsystem", "status", "value", "note"}
	w := make([]int, 4)
	for i, h := range head {
		w[i] = runeLen(h)
	}
	for _, r := range rows {
		w[0] = maxi(w[0], runeLen(r.subsystem))
		w[1] = maxi(w[1], runeLen(r.status))
		w[2] = maxi(w[2], runeLen(r.value))
		w[3] = maxi(w[3], runeLen(r.note))
	}

	fmt.Fprintln(b, boardBorder("┌", "┬", "┐", w))
	fmt.Fprintf(b, "│ %s │ %s │ %s │ %s │\n", padL(head[0], w[0]), padC(head[1], w[1]), padC(head[2], w[2]), padL(head[3], w[3]))
	fmt.Fprintln(b, boardBorder("├", "┼", "┤", w))
	for _, r := range rows {
		status := statusColor(st, r.kind)(padC(r.status, w[1]))
		fmt.Fprintf(b, "│ %s │ %s │ %s │ %s │\n", padL(r.subsystem, w[0]), status, padR(r.value, w[2]), padL(r.note, w[3]))
	}
	fmt.Fprintln(b, boardBorder("└", "┴", "┘", w))
	fmt.Fprintln(b)
}

func boardBorder(left, mid, right string, w []int) string {
	segs := make([]string, len(w))
	for i, ww := range w {
		segs[i] = strings.Repeat("─", ww+2)
	}
	return left + strings.Join(segs, mid) + right
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }
func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func padL(s string, w int) string { return s + strings.Repeat(" ", w-runeLen(s)) }
func padR(s string, w int) string { return strings.Repeat(" ", w-runeLen(s)) + s }
func padC(s string, w int) string {
	pad := w - runeLen(s)
	l := pad / 2
	return strings.Repeat(" ", l) + s + strings.Repeat(" ", pad-l)
}

// buildBoard derives the subsystem rows from the Context. A row's status is
// taken from the finding that governs it (so "locks" reads fail when a blocking
// chain fired), else ok.
func buildBoard(c *model.Context) []boardRow {
	sev := map[string]string{}
	for _, f := range c.Findings {
		sev[f.ID] = f.Severity
	}
	statusFor := func(ids ...string) (string, statusKind) {
		word, kind := "ok", kOK
		for _, id := range ids {
			switch sev[id] {
			case model.SeverityCritical:
				return "fail", kBad
			case model.SeverityWarn:
				word, kind = "warn", kWatch
			case model.SeverityInfo:
				if kind == kOK {
					word, kind = "note", kInfo
				}
			}
		}
		return word, kind
	}

	var rows []boardRow
	if h := c.Health; c.Server.Engine == "cockroachdb" && h != nil && h.Cockroach != nil {
		ch := h.Cockroach
		if h.Exactness != model.ExactnessUnavailable {
			s, k := statusFor("crdb_node_unavailable")
			rows = append(rows, boardRow{"cluster", s, k, fmt.Sprintf("%d/%d live", ch.NodesLive, ch.NodesTotal), fmt.Sprintf("%d stores", ch.StoresTotal)})
			s, k = statusFor("crdb_ranges_unavailable", "crdb_ranges_underreplicated")
			rows = append(rows, boardRow{"ranges", s, k, fmt.Sprintf("%d unavailable", ch.UnavailableRanges), fmt.Sprintf("%d under-replicated", ch.UnderreplicatedRanges)})
			if ch.CapacityBytes > 0 {
				s, k = statusFor("crdb_store_capacity")
				rows = append(rows, boardRow{"capacity", s, k, humanBytes(ch.AvailableBytes) + " free", "fullest " + pct(ch.MaxStoreUsedRatio)})
			}
			if ch.Distribution.Exactness != "" && ch.Distribution.Exactness != model.ExactnessUnavailable {
				d := &ch.Distribution
				s, k = statusFor("crdb_replica_imbalance", "crdb_leaseholder_imbalance", "crdb_capacity_imbalance", "crdb_hot_range_concentration")
				rows = append(rows, boardRow{"balance", s, k, fmt.Sprintf("%d/%d comparable", d.ComparableStores, d.LiveStores), fmt.Sprintf("replicas %d–%d", d.ReplicaMin, d.ReplicaMax)})
			}
			if ch.Storage.Exactness != "" && ch.Storage.Exactness != model.ExactnessUnavailable {
				storage := &ch.Storage
				s, k = statusFor("crdb_external_disk_usage", "crdb_storage_stall", "crdb_replica_size_skew")
				rows = append(rows, boardRow{"storage", s, k, humanBytes(storage.CockroachUsedBytes) + " CRDB", humanBytes(storage.OtherUsedBytes) + " other"})
				s, k = statusFor("crdb_replication_recovery", "crdb_raft_backlog")
				rows = append(rows, boardRow{"replication", s, k, fmt.Sprintf("%s uninitialized", humanNum(float64(storage.UninitializedReplicas))), fmt.Sprintf("%s Raft pending", humanNum(float64(storage.RaftCommandsPending)))})
			}
			if ch.QueriesPerSec != nil {
				s, k = statusFor("crdb_resource_pressure")
				rows = append(rows, boardRow{"load", s, k, humanNum(*ch.QueriesPerSec) + " qps", fmt.Sprintf("%d SQL conns", ch.SQLConnections)})
			}
		}
		if ch.Jobs.Exactness != "" && ch.Jobs.Exactness != model.ExactnessUnavailable {
			counts := countCockroachJobs(ch.JobItems)
			total := ch.JobsTotal
			if total == 0 {
				total = len(ch.JobItems)
			}
			s, k := statusFor("crdb_job_failed", "crdb_job_stalled", "crdb_job_reverting", "crdb_job_paused")
			rows = append(rows, boardRow{"jobs", s, k, fmt.Sprintf("%d tracked", total), fmt.Sprintf("%d need attention", counts.attention)})
		}
	} else if h := c.Health; h != nil && h.Exactness != model.ExactnessUnavailable {
		note := ""
		if c.Activity != nil && c.Activity.IdleInTransaction > 0 {
			note = fmt.Sprintf("%d idle in txn", c.Activity.IdleInTransaction)
		}
		connVal := fmt.Sprintf("%d", h.Connections)
		connStatus, connKind := statusFor("connection_saturation")
		if c.Limits != nil && c.Limits.ConnectionsMax > 0 {
			connVal = fmt.Sprintf("%d/%d", c.Limits.ConnectionsUsed, c.Limits.ConnectionsMax)
		}
		rows = append(rows, boardRow{"connections", connStatus, connKind, connVal, note})
		if h.CacheHitRatio != nil {
			s, k := statusFor("low_cache_hit")
			note := ""
			if !h.CacheHitUsable() {
				// too little block traffic in the window to grade — show the
				// number, not a verdict (see model.CacheHitMinBlocks). PR#1.
				s, k, note = "n/a", kInfo, "thin sample"
			}
			rows = append(rows, boardRow{"cache", s, k, pct(*h.CacheHitRatio), note})
		}
		if h.TPS != nil {
			rows = append(rows, boardRow{"throughput", "ok", kOK, humanNum(*h.TPS) + " tps", ""})
		}
		if h.RollbackRatio != nil {
			s, k := statusFor("high_rollback_ratio")
			note := ""
			if *h.RollbackRatio >= 0.10 {
				note = "app error handling?"
			}
			rows = append(rows, boardRow{"rollbacks", s, k, pct(*h.RollbackRatio), note})
		}
	}
	if a := c.Activity; c.Server.Engine == "cockroachdb" && a != nil && a.Exactness != model.ExactnessUnavailable {
		rows = append(rows, boardRow{"activity", "ok", kOK, fmt.Sprintf("%d sessions", a.Total), fmt.Sprintf("%d active", a.Active)})
	}
	if c.Server.Engine == "cockroachdb" && c.Queries != nil && c.Queries.Exactness != model.ExactnessUnavailable {
		rows = append(rows, boardRow{"query stats", "ok", kOK, fmt.Sprintf("%d fingerprints", len(c.Queries.Top)), cockroachQuerySourceNote(c.Queries)})
	}
	if c.Cockroach != nil && c.Cockroach.ExecutionInsights.Exactness != model.ExactnessUnavailable {
		status, kind := statusFor("crdb_execution_insights")
		rows = append(rows, boardRow{"SQL insights", status, kind, fmt.Sprintf("%d recent", len(c.Cockroach.ExecutionInsights.Items)), "slow / failed"})
	}
	if c.Cockroach != nil && c.Cockroach.Contention.Exactness != "" && c.Cockroach.Contention.Exactness != model.ExactnessUnavailable {
		h := &c.Cockroach.Contention
		status, kind := statusFor("crdb_contention_hotspot", "crdb_serialization_conflicts")
		rows = append(rows, boardRow{"contention", status, kind, fmt.Sprintf("%d events", h.TotalEvents), humanDurationMS(h.TotalWaitMS) + " wait"})
	}
	if c.Locks != nil && c.Locks.Exactness != model.ExactnessUnavailable {
		s, k := statusFor("blocking_chains")
		val, note := "clear", ""
		if c.Locks.BlockedCount > 0 {
			val = fmt.Sprintf("%d blocked", c.Locks.BlockedCount)
			if len(c.Locks.Chains) > 0 {
				note = truncate(fmt.Sprintf("pid %d blocked by %v", c.Locks.Chains[0].BlockedPID, c.Locks.Chains[0].BlockingPIDs), 28)
			}
		}
		rows = append(rows, boardRow{"locks", s, k, val, note})
	}
	if c.Indexes != nil && c.Indexes.Exactness != model.ExactnessUnavailable {
		s, k := statusFor("unused_indexes", "index_invalid", "crdb_unused_indexes")
		if c.Server.Engine == "cockroachdb" {
			val := fmt.Sprintf("%d secondary", c.Indexes.SecondaryTotal)
			note := "none unused ≥" + indexThreshold(c.Indexes)
			if len(c.Indexes.Unused) > 0 {
				val = fmt.Sprintf("%d candidates", len(c.Indexes.Unused))
				note = "no reads ≥" + indexThreshold(c.Indexes)
			}
			rows = append(rows, boardRow{"indexes", s, k, val, note})
		} else {
			var total int64
			for _, ix := range c.Indexes.Unused {
				total += ix.Bytes
			}
			val, note := "clean", ""
			if len(c.Indexes.Unused) > 0 {
				val = HumanBytes(total)
				note = fmt.Sprintf("%d zero scans", len(c.Indexes.Unused))
			}
			rows = append(rows, boardRow{"indexes", s, k, val, note})
		}
	}
	if c.Tables != nil && c.Tables.Exactness != model.ExactnessUnavailable {
		if c.Server.Engine == "cockroachdb" {
			s, k := statusFor("crdb_table_metadata_error", "crdb_table_stats_missing", "crdb_mvcc_garbage_pressure")
			note := fmt.Sprintf("%d tables", c.Tables.Total)
			if age := tableMetadataAge(c); age != "unknown" {
				note += " · cache " + age
			}
			rows = append(rows, boardRow{"tables", s, k, HumanBytes(c.Tables.DBSizeBytes), note})
		} else {
			s, k := statusFor("table_bloat", "seq_scan_heavy")
			rows = append(rows, boardRow{"tables", s, k, HumanBytes(c.Tables.DBSizeBytes), fmt.Sprintf("%d tracked", len(c.Tables.Top))})
		}
	}
	if c.Limits != nil && c.Limits.Exactness != model.ExactnessUnavailable {
		s, k := statusFor("txid_wraparound")
		note := "of 2.1B max"
		if k == kOK {
			note = "no wraparound risk"
		}
		rows = append(rows, boardRow{"wraparound", s, k, humanNum(float64(c.Limits.MaxXIDAge)), note})
	}
	if w := c.WAL; w != nil && w.BytesPerSec != nil {
		rows = append(rows, boardRow{"WAL", "ok", kOK, HumanBytes(int64(*w.BytesPerSec)) + "/s", ""})
	}
	if c.IO != nil && c.IO.Exactness != model.ExactnessUnavailable {
		note := "none forced"
		if c.IO.CheckpointsReq > 0 {
			note = fmt.Sprintf("%d forced", c.IO.CheckpointsReq)
		}
		rows = append(rows, boardRow{"checkpoints", "ok", kOK, "timed", note})
	}
	if r := c.Replication; r != nil && r.Exactness != model.ExactnessUnavailable {
		val, note := "none", ""
		switch {
		case r.IsReplica:
			val = "replica"
			if r.ReceiverLagSec != nil {
				val = fmt.Sprintf("%.0f ms", *r.ReceiverLagSec*1000)
			}
		case len(r.Replicas) > 0:
			val = fmt.Sprintf("%d up", len(r.Replicas))
			note = "streaming"
		}
		rows = append(rows, boardRow{"replication", "ok", kOK, val, note})
	}
	if c.Settings != nil && c.Settings.Exactness != model.ExactnessUnavailable {
		rows = append(rows, boardRow{"settings", "ok", kOK, fmt.Sprintf("%d non-default", len(c.Settings.Overrides)), ""})
	}
	return rows
}
