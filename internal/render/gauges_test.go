package render

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pgrundev/pgbot/internal/model"
)

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

// gaugeContext is a busy database: every gauge measurable, two of them hot.
func gaugeContext() *model.Context {
	c := sampleContext()
	c.Health.RollbackRatio = f64(0.12)
	c.Locks = &model.Locks{BlockedCount: 2}
	c.WaitProfile = &model.WaitProfile{
		Available: true, Samples: 100, WindowSeconds: 10,
		Buckets: []model.WaitBucket{{Type: "Lock", Count: 61, Share: 0.61}, {Type: "CPU", Count: 39, Share: 0.39}},
		ByQuery: []model.QueryWaits{
			{QueryID: 0x1111000000000000, Count: 30, Share: 0.3, LockShare: 0.2},
			{QueryID: 0x4f2a000000000000, Count: 40, Share: 0.4, LockShare: 0.9},
		},
	}
	c.Tables = &model.Tables{DBSizeBytes: 100 << 30}
	c.Indexes = &model.Indexes{Total: 12, Unused: []model.IndexStat{
		{Name: "a", Scans: 0, Bytes: 40 << 30},
		{Name: "b", Scans: 0, Bytes: 3 << 30},
		{Name: "c", Scans: 5, Bytes: 9 << 30}, // scanned — not idle
	}}
	c.Findings = append(c.Findings, model.Finding{ID: "high_rollback_ratio", Severity: model.SeverityWarn, Title: "rollbacks 12%"})
	return c
}

func TestGauge_fillRoundsToNearestCellWithOneCellMinimum(t *testing.T) {
	cases := []struct {
		share float64
		cells int
	}{{0, 0}, {0.001, 1}, {0.024, 1}, {0.026, 1}, {0.074, 1}, {0.076, 2}, {0.5, 10}, {0.974, 19}, {0.976, 20}, {1, 20}, {1.4, 20}}
	for _, tc := range cases {
		if got := gaugeCells(tc.share); got != tc.cells {
			t.Errorf("gaugeCells(%v) = %d, want %d", tc.share, got, tc.cells)
		}
	}
	if got := strings.Count(gaugeBar(0.5), "█"); got != 10 {
		t.Errorf("bar at 0.5 has %d filled cells, want 10", got)
	}
	if got := utf8.RuneCountInString(gaugeBar(0.3)); got != gaugeWidth {
		t.Errorf("bar is %d runes wide, want %d", got, gaugeWidth)
	}
}

func TestGauge_cacheHit(t *testing.T) {
	c := gaugeContext()
	g := cacheHitGauge(c)
	if g.value != "99.4%" || g.status != "ok" || g.kind != kOK || g.measurable != true {
		t.Errorf("healthy cache hit: %+v", g)
	}
	c.Findings = append(c.Findings, model.Finding{ID: "low_cache_hit", Severity: model.SeverityWarn})
	if g := cacheHitGauge(c); g.status != "low" || g.kind != kBad {
		t.Errorf("low cache hit should read low/red: %+v", g)
	}
	c.Health.CacheBlocks = i64(100)
	if g := cacheHitGauge(c); g.measurable || g.value != "—" || g.status != "thin sample" {
		t.Errorf("thin sample must not be graded: %+v", g)
	}
	c.Health = nil
	if g := cacheHitGauge(c); g.measurable {
		t.Errorf("no health section must be not measurable: %+v", g)
	}
}

func TestGauge_lockWait(t *testing.T) {
	c := gaugeContext()
	g := lockWaitGauge(c)
	if g.value != "61.0%" || g.share != 0.61 || g.status != "query 4f2a" || g.kind != kBad {
		t.Errorf("blocked with attribution should name the culprit: %+v", g)
	}
	c.WaitProfile.ByQuery = nil
	if g := lockWaitGauge(c); g.status != "2 blocked" || g.kind != kBad {
		t.Errorf("blocked without attribution falls back to the count: %+v", g)
	}
	c.Locks.BlockedCount = 0
	if g := lockWaitGauge(c); g.status != "ok" || g.kind != kOK || g.value != "61.0%" {
		t.Errorf("no blocked sessions is ok even with lock samples: %+v", g)
	}
	c.WaitProfile = nil
	if g := lockWaitGauge(c); g.value != "—" || g.status != "ok" || g.share != 0 {
		t.Errorf("no wait profile shows only the status: %+v", g)
	}
	c.Locks = nil
	if g := lockWaitGauge(c); g.measurable {
		t.Errorf("neither profile nor locks is not measurable: %+v", g)
	}
	// A negative query_id (int64 hash) still renders as 4 hex digits.
	c = gaugeContext()
	c.WaitProfile.ByQuery = []model.QueryWaits{{QueryID: -1, Count: 1, Share: 1, LockShare: 1}}
	if g := lockWaitGauge(c); g.status != "query ffff" {
		t.Errorf("negative query id: %+v", g)
	}
}

func TestGauge_rollbacks(t *testing.T) {
	c := gaugeContext()
	if g := rollbacksGauge(c); g.value != "12.0%" || g.status != "watch" || g.kind != kWatch {
		t.Errorf("rollback finding fired should read watch: %+v", g)
	}
	c.Findings = nil
	if g := rollbacksGauge(c); g.status != "ok" || g.kind != kOK {
		t.Errorf("no finding is ok: %+v", g)
	}
	c.Health.RollbackRatio = nil
	if g := rollbacksGauge(c); g.measurable || g.value != "—" || g.status != "not measurable" {
		t.Errorf("nil ratio is not measurable: %+v", g)
	}
}

func TestGauge_idleIndexes(t *testing.T) {
	c := gaugeContext()
	g := idleIndexGauge(c)
	// 43 GiB of zero-scan indexes over a 100 GiB database: 0.43 → 9 cells.
	if g.value != "43.0 GiB" || gaugeCells(g.share) != 9 || g.status != "review" || g.kind != kWatch {
		t.Errorf("unused indexes fired: %+v", g)
	}
	c.Findings = nil
	if g := idleIndexGauge(c); g.status != "ok" || g.kind != kOK || g.value != "43.0 GiB" {
		t.Errorf("below threshold is ok but still sized: %+v", g)
	}
	c.Indexes.Unused = nil
	if g := idleIndexGauge(c); g.value != "0 B" || g.status != "ok" || g.share != 0 {
		t.Errorf("zero idle bytes: %+v", g)
	}
	c.Tables = nil
	c.Indexes = gaugeContext().Indexes
	if g := idleIndexGauge(c); g.share != 0 || g.value != "43.0 GiB" {
		t.Errorf("no database size: value stays, fill is empty: %+v", g)
	}
	c.Window.WindowAgeSeconds = i64(120)
	if g := idleIndexGauge(c); g.measurable || g.status != "window < 15m" {
		t.Errorf("cold window: %+v", g)
	}
	c.Window.WindowAgeSeconds = nil
	c.Indexes = nil
	if g := idleIndexGauge(c); g.measurable || g.status != "not measurable" {
		t.Errorf("no index section: %+v", g)
	}
}

func TestGaugeStrip_layoutNoColorAndWidth(t *testing.T) {
	var b strings.Builder
	renderGauges(&b, styler{on: false}, gaugeContext(), 80)
	out := b.String()
	if regexp.MustCompile("\x1b\\[").MatchString(out) {
		t.Error("no-color strip must contain no ANSI escapes")
	}
	want := []string{
		"  cache hit  [████████████████████]  99.4%     ok",
		"  lock wait  [████████████░░░░░░░░]  61.0%     query 4f2a",
		"  rollbacks  [██░░░░░░░░░░░░░░░░░░]  12.0%     watch",
		"  idle idx   [█████████░░░░░░░░░░░]  43.0 GiB  review",
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != len(want) {
		t.Fatalf("strip has %d lines, want %d:\n%s", len(lines), len(want), out)
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("line %d:\n got %q\nwant %q", i, lines[i], w)
		}
		if n := utf8.RuneCountInString(lines[i]); n > 80 {
			t.Errorf("line %d is %d columns wide, over the 80-column floor", i, n)
		}
	}
	// Colour on: the block characters survive so the bar reads with color stripped.
	b.Reset()
	renderGauges(&b, styler{on: true}, gaugeContext(), 80)
	if !strings.Contains(b.String(), "█") || !strings.Contains(b.String(), "query 4f2a") {
		t.Error("colored strip lost its bar or status")
	}
}

func TestGaugeStrip_notMeasurableRowsAreDim(t *testing.T) {
	var b strings.Builder
	renderGauges(&b, styler{on: false}, sampleContext(), 80)
	out := b.String()
	// sampleContext has cache hit only: the other three rows are empty bars with "—".
	for _, w := range []string{"cache hit  [████████████████████]  99.4%", "lock wait  [░░░░░░░░░░░░░░░░░░░░]  —", "rollbacks  [░░░░░░░░░░░░░░░░░░░░]  —", "idle idx   [░░░░░░░░░░░░░░░░░░░░]  —"} {
		if !strings.Contains(out, w) {
			t.Errorf("strip missing %q in:\n%s", w, out)
		}
	}
}

func TestChecked_orderExclusionAndWrap(t *testing.T) {
	c := gaugeContext()
	c.Queries = &model.Queries{Enabled: true}
	c.Replication = &model.Replication{}
	c.WAL = &model.WAL{}
	c.Limits = &model.Limits{ConnectionsMax: 100, ConnectionsUsed: 10, MaxXIDAge: 1000}
	c.Settings = &model.Settings{}
	c.Health.DeadlocksPerMin = f64(0)
	// unused_indexes fired in gaugeContext → indexes is left out; everything else is clean.
	got := buildChecked(c)
	want := []string{"queries", "vacuum", "replication", "checkpoints", "connections", "settings", "wraparound", "deadlocks"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("checked = %v, want %v", got, want)
	}
	// A finding in a subsystem removes it, wherever it sits in the order.
	c.Findings = append(c.Findings, model.Finding{ID: "replica_lag_time", Severity: model.SeverityWarn})
	c.Health.DeadlocksPerMin = f64(0.5)
	got = buildChecked(c)
	for _, s := range got {
		if s == "replication" || s == "deadlocks" {
			t.Errorf("%q must not be listed as checked-clean: %v", s, got)
		}
	}
	// Nothing collected → nothing claimed.
	if got := buildChecked(sampleContext()); len(got) != 0 {
		t.Errorf("bare context claims %v", got)
	}

	var b strings.Builder
	renderChecked(&b, styler{on: false}, want, 60)
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected wrapping at width 60:\n%s", b.String())
	}
	if !strings.HasPrefix(lines[0], "checked · queries") {
		t.Errorf("first line: %q", lines[0])
	}
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "        · ") {
			t.Errorf("continuation lacks the 8-space hanging indent: %q", l)
		}
	}
	for _, l := range lines {
		if n := utf8.RuneCountInString(l); n > 60 {
			t.Errorf("line over width: %d %q", n, l)
		}
	}
}

func TestGrouped_stripReplacesGoodAndSkipsSchema(t *testing.T) {
	var b strings.Builder
	renderGrouped(&b, styler{on: false}, gaugeContext(), 100)
	out := b.String()
	if strings.Contains(out, "GOOD") {
		t.Error("GOOD block should be gone")
	}
	for _, w := range []string{"cache hit  [", "rollbacks  [", "Database health:", `pgbot ask "why is it slow?"`} {
		if !strings.Contains(out, w) {
			t.Errorf("grouped view missing %q:\n%s", w, out)
		}
	}
	// The strip comes before the score.
	if strings.Index(out, "cache hit") > strings.Index(out, "Database health:") {
		t.Error("strip must sit above the score")
	}
	c := gaugeContext()
	c.Profile = "schema"
	b.Reset()
	renderGrouped(&b, styler{on: false}, c, 100)
	if strings.Contains(b.String(), "cache hit  [") || strings.Contains(b.String(), "checked ·") {
		t.Error("schema profile must not render the strip or the checked line")
	}
}
