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
			}
		}
		return word, kind
	}

	var rows []boardRow
	if h := c.Health; h != nil {
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
				// number, not a verdict (see model.CacheHitMinBlocks)
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
	if c.Locks != nil {
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
	if c.Indexes != nil {
		s, k := statusFor("unused_indexes", "index_invalid")
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
	if c.Tables != nil {
		s, k := statusFor("table_bloat", "seq_scan_heavy")
		rows = append(rows, boardRow{"tables", s, k, HumanBytes(c.Tables.DBSizeBytes), fmt.Sprintf("%d tracked", len(c.Tables.Top))})
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
	if c.IO != nil {
		note := "none forced"
		if c.IO.CheckpointsReq > 0 {
			note = fmt.Sprintf("%d forced", c.IO.CheckpointsReq)
		}
		rows = append(rows, boardRow{"checkpoints", "ok", kOK, "timed", note})
	}
	if r := c.Replication; r != nil {
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
	if c.Settings != nil {
		rows = append(rows, boardRow{"settings", "ok", kOK, fmt.Sprintf("%d non-default", len(c.Settings.Overrides)), ""})
	}
	return rows
}
