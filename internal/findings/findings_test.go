package findings

import (
	"testing"
	"unicode/utf8"

	"github.com/pgrundev/pgbot/internal/model"
)

func ptr(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

func has(fs []model.Finding, id string) *model.Finding {
	for i := range fs {
		if fs[i].ID == id {
			return &fs[i]
		}
	}
	return nil
}

func TestCompute_flagsRealIssues(t *testing.T) {
	c := &model.Context{
		Window:   model.Window{StatsWindowDays: ptr(120), SampleSeconds: 1},
		Health:   &model.Health{CacheHitRatio: ptr(0.80), CacheBlocks: i64(50_000), RollbackRatio: ptr(0.15), TPS: ptr(200)}, // enough volume to trust the ratios
		Activity: &model.Activity{IdleInTransaction: 2, LongestXactSec: 400},
		Locks:    &model.Locks{BlockedCount: 1, Chains: []model.BlockingRow{{BlockedPID: 42, WaitSeconds: 30}}},
		Indexes: &model.Indexes{Unused: []model.IndexStat{
			{Schema: "public", Table: "orders", Name: "big_idx", Bytes: 5 << 20},
		}},
		Tables: &model.Tables{Top: []model.TableStat{
			{Schema: "public", Name: "orders", DeadRatio: 0.35, LiveTuples: 90000, DeadTuples: 48000, ModsSinceAnalyze: 5000},
		}},
		Queries: &model.Queries{Enabled: true},
	}
	fs := Compute(c)

	for _, id := range []string{"blocking_chains", "unused_indexes", "table_bloat", "low_cache_hit", "idle_in_transaction", "long_running_transaction", "high_rollback_ratio", "stale_stats_window"} {
		if has(fs, id) == nil {
			t.Errorf("expected finding %q", id)
		}
	}
	// critical must sort first.
	if len(fs) == 0 || fs[0].Severity != model.SeverityCritical {
		t.Errorf("expected a critical finding first, got %+v", fs)
	}
}

func TestCompute_cleanDatabaseHasNoFalsePositives(t *testing.T) {
	c := &model.Context{
		Health:   &model.Health{CacheHitRatio: ptr(0.999), RollbackRatio: ptr(0.001)},
		Activity: &model.Activity{IdleInTransaction: 0},
		Locks:    &model.Locks{BlockedCount: 0},
		Indexes:  &model.Indexes{},
		Tables:   &model.Tables{},
		Queries:  &model.Queries{Enabled: true},
	}
	if fs := Compute(c); len(fs) != 0 {
		t.Errorf("clean database should have no findings, got %+v", fs)
	}
}

func TestCompute_missingPgssIsInfo(t *testing.T) {
	c := &model.Context{Queries: &model.Queries{Enabled: false}}
	f := has(Compute(c), "pg_stat_statements_missing")
	if f == nil || f.Severity != model.SeverityInfo {
		t.Fatalf("expected info finding for missing pgss, got %+v", f)
	}
}

func TestComputeCockroachDoesNotRecommendPgStatStatements(t *testing.T) {
	c := &model.Context{Server: model.ServerInfo{Engine: "cockroachdb"}, Queries: &model.Queries{Enabled: false}}
	if f := has(Compute(c), "pg_stat_statements_missing"); f != nil {
		t.Fatalf("CockroachDB must not receive PostgreSQL extension advice: %+v", f)
	}
}

func TestColdWindow_suppressesCounterFindings_keepsGauges(t *testing.T) {
	cold := int64(120) // 2 min — below the 900s threshold
	c := &model.Context{
		Window: model.Window{WindowAgeSeconds: &cold},
		Health: &model.Health{CacheHitRatio: ptr(0.50), CacheBlocks: i64(50_000)}, // would fire low_cache_hit on a warm window
		Indexes: &model.Indexes{Unused: []model.IndexStat{
			{Schema: "public", Table: "orders", Name: "big_idx", Bytes: 50 << 20},
		}},
		Tables: &model.Tables{Top: []model.TableStat{
			{Schema: "public", Name: "orders", LiveTuples: 1_000_000, SeqScans: 9000, IndexScans: 10},
		}},
		// Gauges — must still fire on a cold window:
		Locks:   &model.Locks{BlockedCount: 1, Chains: []model.BlockingRow{{BlockedPID: 7, WaitSeconds: 12}}},
		Queries: &model.Queries{Enabled: true},
		Schema:  &model.SchemaFingerprint{Objects: []model.SchemaObject{{Kind: "index", Identity: "public.t.bad", Invalid: true}}},
	}
	fs := Compute(c)
	for _, suppressed := range []string{"unused_indexes", "low_cache_hit", "seq_scan_heavy"} {
		if has(fs, suppressed) != nil {
			t.Errorf("cold window must suppress %q", suppressed)
		}
	}
	if has(fs, "blocking_chains") == nil {
		t.Error("blocking chains is a gauge and must still fire on a cold window")
	}
	if has(fs, "index_invalid") == nil {
		t.Error("index_invalid is a gauge and must still fire on a cold window")
	}
}

// T12 #5: a failed CREATE INDEX CONCURRENTLY (indisvalid=false) fires
// index_invalid as a critical gauge.
func TestIndexInvalid_firesCritical(t *testing.T) {
	c := &model.Context{Schema: &model.SchemaFingerprint{Objects: []model.SchemaObject{
		{Kind: "index", Identity: "public.orders.orders_uidx", Invalid: true, IndexReady: true, IndexLive: true},
		{Kind: "index", Identity: "public.orders.orders_pkey", Invalid: false, IndexReady: true, IndexLive: true}, // valid, must be ignored
	}}}
	f := has(Compute(c), "index_invalid")
	if f == nil {
		t.Fatal("an invalid index must fire index_invalid")
	}
	if f.Severity != model.SeverityCritical {
		t.Errorf("index_invalid should be critical, got %s", f.Severity)
	}
	if f.Impact.Dimension != model.DimRisk {
		t.Errorf("index_invalid should be a risk, got %s", f.Impact.Dimension)
	}
}

func TestSeqScanHeavy_firesOnWarmWindow(t *testing.T) {
	warm := int64(7200)
	c := &model.Context{
		Window: model.Window{WindowAgeSeconds: &warm},
		Tables: &model.Tables{Top: []model.TableStat{
			{Schema: "public", Name: "orders", LiveTuples: 1_000_000, SeqScans: 9000, IndexScans: 10},
		}},
		Queries: &model.Queries{Enabled: true},
	}
	if has(Compute(c), "seq_scan_heavy") == nil {
		t.Error("expected seq_scan_heavy on a large seq-scanned table over a warm window")
	}
}

func TestUnusedIndexes_belowThresholdIgnored(t *testing.T) {
	c := &model.Context{Indexes: &model.Indexes{Unused: []model.IndexStat{
		{Schema: "public", Table: "t", Name: "tiny", Bytes: 100 << 10}, // 100 KiB < 1 MiB
	}}}
	if has(Compute(c), "unused_indexes") != nil {
		t.Error("a sub-threshold unused index must not be flagged")
	}
}

// --- T8: wait-profile findings ---

func waitProfile(samples int, buckets []model.WaitBucket, byQuery []model.QueryWaits) *model.WaitProfile {
	return &model.WaitProfile{Available: true, Samples: samples, WindowSeconds: 5, Buckets: buckets, ByQuery: byQuery}
}

func TestWaitFindings_thinProfileFiresNothing(t *testing.T) {
	// 10 samples (< WaitMinSamples) all on locks: must NOT fire — it's noise.
	c := &model.Context{WaitProfile: waitProfile(10,
		[]model.WaitBucket{{Type: "Lock", Count: 10, Share: 1.0, Events: []model.WaitEvent{{Event: "transactionid", Count: 10, Share: 1.0}}}},
		[]model.QueryWaits{{QueryID: 5, Count: 10, Share: 1.0, LockShare: 1.0}},
	)}
	fs := Compute(c)
	if has(fs, "wait_lock_contention") != nil {
		t.Error("thin profile (<20 samples) must not fire wait findings")
	}
}

func TestWaitFindings_lockContention(t *testing.T) {
	c := &model.Context{WaitProfile: waitProfile(50,
		[]model.WaitBucket{
			{Type: "Lock", Count: 30, Share: 0.6, Events: []model.WaitEvent{{Event: "transactionid", Count: 30, Share: 0.6}}},
			{Type: "CPU", Count: 20, Share: 0.4},
		},
		[]model.QueryWaits{{QueryID: 4242, SampleText: "UPDATE orders SET ...", Count: 30, Share: 0.6, LockShare: 1.0, TopType: "Lock", TopEvent: "Lock:transactionid"}},
	)}
	f := has(Compute(c), "wait_lock_contention")
	if f == nil {
		t.Fatal("a query >30% on locks (with enough samples) should fire wait_lock_contention")
	}
	if f.Severity != model.SeverityWarn {
		t.Errorf("want warn, got %s", f.Severity)
	}
}

func TestWaitFindings_ioBound(t *testing.T) {
	c := &model.Context{WaitProfile: waitProfile(40,
		[]model.WaitBucket{
			{Type: "IO", Count: 24, Share: 0.6, Events: []model.WaitEvent{{Event: "DataFileRead", Count: 24, Share: 0.6}}},
			{Type: "CPU", Count: 16, Share: 0.4},
		}, nil,
	)}
	if has(Compute(c), "wait_io_bound") == nil {
		t.Error(">50% IO should fire wait_io_bound")
	}
}

func TestWaitFindings_idleDatabaseSilent(t *testing.T) {
	// 0 samples: an idle database. No wait findings.
	c := &model.Context{WaitProfile: waitProfile(0, nil, nil)}
	for _, id := range []string{"wait_lock_contention", "wait_io_bound", "wait_lwlock_pressure"} {
		if has(Compute(c), id) != nil {
			t.Errorf("idle database must not fire %s", id)
		}
	}
}

// --- T9: impact scoring, confidence, caveats ---

func longWindow() model.Window {
	age := int64(30 * 24 * 3600) // 30 days: not cold, not short
	return model.Window{WindowAgeSeconds: &age}
}

func TestUnusedIndex_scoreDrivenBySizeAndWrite(t *testing.T) {
	c := &model.Context{
		Window: longWindow(),
		Tables: &model.Tables{Top: []model.TableStat{
			{Schema: "public", Name: "orders", ModsSinceAnalyze: 500000}, // write-heavy
			{Schema: "public", Name: "countries", ModsSinceAnalyze: 0},   // static
		}},
		Indexes: &model.Indexes{Unused: []model.IndexStat{
			{Schema: "public", Table: "countries", Name: "small_idx", Bytes: 2 << 20},   // 2 MiB, static
			{Schema: "public", Table: "orders", Name: "big_idx", Bytes: 12 * (1 << 30)}, // 12 GiB, write-heavy
		}},
	}
	f := has(Compute(c), "unused_indexes")
	if f == nil {
		t.Fatal("expected unused_indexes finding")
	}
	if f.Impact.Dimension != model.DimStorage {
		t.Errorf("dimension should be storage, got %q", f.Impact.Dimension)
	}
	// Evidence must lead with the 12 GiB write-heavy index (highest score).
	if len(f.Evidence) == 0 || !contains(f.Evidence[0], "big_idx") {
		t.Errorf("evidence should lead with the 12 GiB write-heavy index, got %v", f.Evidence)
	}
	if f.Impact.Score < 90 {
		t.Errorf("a 12 GiB write-heavy unused index should score high, got %.1f", f.Impact.Score)
	}
}

func TestUnusedIndex_replicationCaveatMandatory(t *testing.T) {
	c := &model.Context{
		Window:      longWindow(),
		Replication: &model.Replication{Replicas: []model.ReplicaRow{{ClientAddr: "10.0.0.2", State: "streaming"}}},
		Indexes:     &model.Indexes{Unused: []model.IndexStat{{Schema: "public", Table: "t", Name: "idx", Bytes: 50 << 20}}},
	}
	f := has(Compute(c), "unused_indexes")
	if f == nil {
		t.Fatal("expected unused_indexes finding")
	}
	found := false
	for _, cav := range f.Caveats {
		if contains(cav, "replica") {
			found = true
		}
	}
	if !found {
		t.Errorf("replication MUST add a per-node caveat, got caveats %v", f.Caveats)
	}
}

func TestUnusedIndex_shortWindowLowConfidence(t *testing.T) {
	age := int64(2 * 24 * 3600) // 2 days: not cold (>900s) but < 7d
	c := &model.Context{
		Window:  model.Window{WindowAgeSeconds: &age},
		Indexes: &model.Indexes{Unused: []model.IndexStat{{Schema: "public", Table: "t", Name: "idx", Bytes: 50 << 20}}},
	}
	f := has(Compute(c), "unused_indexes")
	if f == nil {
		t.Fatal("expected unused_indexes finding")
	}
	if f.Confidence > 0.4 {
		t.Errorf("a < 7-day window should cap confidence at 0.4, got %.2f", f.Confidence)
	}
}

func TestUnusedIndex_partialAndExpressionCaveats(t *testing.T) {
	c := &model.Context{
		Window: longWindow(),
		Indexes: &model.Indexes{Unused: []model.IndexStat{
			{Schema: "public", Table: "t", Name: "pidx", Bytes: 40 << 20, Partial: true},
			{Schema: "public", Table: "t", Name: "eidx", Bytes: 40 << 20, Expression: true},
		}},
	}
	f := has(Compute(c), "unused_indexes")
	if f == nil {
		t.Fatal("expected unused_indexes finding")
	}
	joined := ""
	for _, cav := range f.Caveats {
		joined += cav + "\n"
	}
	if !contains(joined, "partial") || !contains(joined, "expression") {
		t.Errorf("partial/expression indexes should each add a caveat, got %v", f.Caveats)
	}
}

func TestOrdering_riskPinnedThenScore(t *testing.T) {
	c := &model.Context{
		Window:  longWindow(),
		Locks:   &model.Locks{BlockedCount: 1, Chains: []model.BlockingRow{{BlockedPID: 9, WaitSeconds: 5}}},
		Indexes: &model.Indexes{Unused: []model.IndexStat{{Schema: "public", Table: "t", Name: "idx", Bytes: 12 * (1 << 30)}}},
	}
	fs := Compute(c)
	if len(fs) < 2 {
		t.Fatalf("expected at least 2 findings, got %d", len(fs))
	}
	// blocking_chains is a risk dimension → pinned above the (high-score) storage win.
	if fs[0].ID != "blocking_chains" {
		t.Errorf("risk finding should be pinned to the top, got %q first", fs[0].ID)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestHighRollbacks_gatedOnVolume(t *testing.T) {
	// 50% rollback ratio but only ~3 transactions in the window → noise, no finding.
	noisy := &model.Context{Window: model.Window{SampleSeconds: 1},
		Health: &model.Health{RollbackRatio: ptr(0.50), TPS: ptr(3)}}
	if has(Compute(noisy), "high_rollback_ratio") != nil {
		t.Error("a ratio over a handful of transactions must not fire (noise)")
	}
	// Same ratio with real volume → fires.
	busy := &model.Context{Window: model.Window{SampleSeconds: 1},
		Health: &model.Health{RollbackRatio: ptr(0.50), TPS: ptr(200)}}
	if has(Compute(busy), "high_rollback_ratio") == nil {
		t.Error("a high ratio over real volume should fire")
	}
}

func TestConnectionSaturation(t *testing.T) {
	mk := func(used, max int) []model.Finding {
		return Compute(&model.Context{Limits: &model.Limits{ConnectionsUsed: used, ConnectionsMax: max}})
	}
	if has(mk(80, 100), "connection_saturation") != nil {
		t.Error("80% must not fire")
	}
	if f := has(mk(90, 100), "connection_saturation"); f == nil || f.Severity != model.SeverityWarn {
		t.Errorf("90%% should warn, got %+v", f)
	}
	if f := has(mk(97, 100), "connection_saturation"); f == nil || f.Severity != model.SeverityCritical {
		t.Errorf("97%% should be critical, got %+v", f)
	}
}

func TestTxidWraparound(t *testing.T) {
	mk := func(age int64) []model.Finding {
		return Compute(&model.Context{Limits: &model.Limits{MaxXIDAge: age}})
	}
	if has(mk(200_000_000), "txid_wraparound") != nil {
		t.Error("200M (healthy, autovacuum territory) must not fire")
	}
	if f := has(mk(1_200_000_000), "txid_wraparound"); f == nil || f.Severity != model.SeverityWarn {
		t.Errorf("1.2B should warn, got %+v", f)
	}
	if f := has(mk(1_900_000_000), "txid_wraparound"); f == nil || f.Severity != model.SeverityCritical {
		t.Errorf("1.9B should be critical, got %+v", f)
	}
	// Both are risks → pinned to the top of the report.
	if f := has(mk(1_900_000_000), "txid_wraparound"); f != nil && f.Impact.Dimension != model.DimRisk {
		t.Errorf("wraparound should be a risk dimension, got %s", f.Impact.Dimension)
	}
}

func TestQuerySlowdown(t *testing.T) {
	pc := func(chg []model.Delta) *model.Context { return &model.Context{Deltas: &model.Deltas{Changes: chg}} }
	// 8ms → 26ms = 3.25× slower on a meaningful query → fires.
	f := has(Compute(pc([]model.Delta{{ID: "query.mean_ms", Subject: "4242", Before: 8, After: 26}})), "query_slowdown")
	if f == nil || f.Severity != model.SeverityWarn || f.Impact.Dimension != model.DimLatency {
		t.Fatalf("a 3.25x slowdown should fire a latency warning, got %+v", f)
	}
	// A micro-query (0.1ms → 0.4ms) is noise even at 4× → no finding.
	if has(Compute(pc([]model.Delta{{ID: "query.mean_ms", Subject: "1", Before: 0.1, After: 0.4}})), "query_slowdown") != nil {
		t.Error("a sub-10ms query must not fire regardless of factor")
	}
	// A small regression (12ms → 15ms = 1.25×) is under the 2× floor → no finding.
	if has(Compute(pc([]model.Delta{{ID: "query.mean_ms", Subject: "2", Before: 12, After: 15}})), "query_slowdown") != nil {
		t.Error("under 2x must not fire")
	}
}

func TestTuning_workMemLow(t *testing.T) {
	spill := 4.0 * (1 << 20) // 4 MiB/s temp files
	c := &model.Context{
		Health:   &model.Health{TempBytesPerSec: &spill},
		Settings: &model.Settings{Params: map[string]string{"work_mem": "4MB"}},
	}
	f := has(Compute(c), "work_mem_low")
	if f == nil || f.Severity != model.SeverityWarn {
		t.Fatalf("temp spilling should recommend raising work_mem, got %+v", f)
	}
	if !contains(f.Title, "4MB") {
		t.Errorf("should name the current work_mem, got %q", f.Title)
	}
	// No spill → no finding.
	none := 0.0
	if has(Compute(&model.Context{Health: &model.Health{TempBytesPerSec: &none}}), "work_mem_low") != nil {
		t.Error("no temp spill must not fire")
	}
}

func TestTuning_checkpointsForced(t *testing.T) {
	c := &model.Context{
		IO:       &model.IO{CheckpointsReq: 40, CheckpointsTimed: 10}, // 80% forced
		Settings: &model.Settings{Params: map[string]string{"max_wal_size": "1GB"}},
	}
	if f := has(Compute(c), "checkpoints_forced"); f == nil || !contains(f.Remediation, "1GB") {
		t.Fatalf("mostly-forced checkpoints should recommend raising max_wal_size, got %+v", f)
	}
	// Too few checkpoints to judge → no finding.
	if has(Compute(&model.Context{IO: &model.IO{CheckpointsReq: 3, CheckpointsTimed: 1}}), "checkpoints_forced") != nil {
		t.Error("under the min-count must not fire")
	}
}

func TestTuning_connectionsOverprovisioned(t *testing.T) {
	c := &model.Context{Limits: &model.Limits{ConnectionsUsed: 20, ConnectionsMax: 500}}
	if f := has(Compute(c), "connections_overprovisioned"); f == nil || f.Severity != model.SeverityInfo {
		t.Fatalf("20/500 should flag over-provisioning as info, got %+v", f)
	}
	// Small max_connections → not flagged.
	if has(Compute(&model.Context{Limits: &model.Limits{ConnectionsUsed: 5, ConnectionsMax: 100}}), "connections_overprovisioned") != nil {
		t.Error("max_connections below the floor must not fire")
	}
}

// TestLowCacheHit_needsBlockTraffic: a cache-hit ratio measured over a few hundred
// blocks is noise (one cold read swings it by tens of points), so the finding must
// not fire — and must not flip the exit code — until the window carries
// model.CacheHitMinBlocks of traffic (PR#1).
func TestLowCacheHit_needsBlockTraffic(t *testing.T) {
	thin := &model.Context{Health: &model.Health{CacheHitRatio: ptr(0.40), CacheBlocks: i64(model.CacheHitMinBlocks - 1)}}
	if has(Compute(thin), "low_cache_hit") != nil {
		t.Fatalf("low_cache_hit fired on %d blocks — below the %d floor", model.CacheHitMinBlocks-1, model.CacheHitMinBlocks)
	}
	unknown := &model.Context{Health: &model.Health{CacheHitRatio: ptr(0.40)}} // no denominator recorded
	if has(Compute(unknown), "low_cache_hit") != nil {
		t.Fatal("low_cache_hit fired without a recorded block count")
	}
	enough := &model.Context{Health: &model.Health{CacheHitRatio: ptr(0.40), CacheBlocks: i64(model.CacheHitMinBlocks)}}
	if has(Compute(enough), "low_cache_hit") == nil {
		t.Fatal("low_cache_hit must fire at the floor with a 40% ratio")
	}
}

// truncate must cut by RUNE, not byte: a byte slice can split a multibyte
// character and emit invalid UTF-8 into the JSON output.
func TestTruncate_runeSafe(t *testing.T) {
	// ASCII: unchanged under the limit, cut + ellipsis over it.
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("short ASCII must be unchanged, got %q", got)
	}
	if got := truncate("hello world", 6); got != "hello…" {
		t.Errorf("ASCII over the limit must cut to 5 runes + ellipsis, got %q", got)
	}
	// Multibyte: each '日' is 3 bytes. Truncating at 4 runes must yield 3 runes
	// + ellipsis and stay valid UTF-8 — the old byte slice split a character.
	in := "日本語のテキスト" // 8 runes, 24 bytes
	got := truncate(in, 4)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate produced invalid UTF-8: %q", got)
	}
	if got != "日本語…" {
		t.Errorf("multibyte must cut on rune boundaries, got %q", got)
	}
	// A string of exactly n runes is returned whole.
	if got := truncate("abcd", 4); got != "abcd" {
		t.Errorf("exactly n runes must be returned whole, got %q", got)
	}
}
