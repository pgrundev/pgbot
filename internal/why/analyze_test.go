package why

import (
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

// history builds hourly samples from per-snapshot builders.
func history(n int, build func(i int, c *model.Context)) []Sample {
	t0 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	out := make([]Sample, n)
	for i := 0; i < n; i++ {
		c := &model.Context{Server: model.ServerInfo{Database: "app"}}
		build(i, c)
		out[i] = Sample{At: t0.Add(time.Duration(i) * time.Hour), C: c}
	}
	return out
}

// The flagship chain: orders queries slowed 3.2x BECAUSE seq scans surged on
// orders AFTER the table grew ~18%. Interval mean (Δtotal/Δcalls) jumps at
// snapshot 3; the seq-scan rate jumps with it; the size grows steadily before.
func flagshipHistory() []Sample {
	return history(6, func(i int, c *model.Context) {
		calls := int64(1000 * (i + 1))
		var totalMS float64
		if i < 3 {
			totalMS = 8 * float64(calls) // 8ms per call so far
		} else {
			totalMS = 8*3000 + 26*float64(calls-3000) // 26ms per call after
		}
		c.Queries = &model.Queries{Enabled: true, TotalExecMS: totalMS, Top: []model.QueryStat{{
			QueryID: 42, Query: "SELECT * FROM orders WHERE customer_id = $1",
			Calls: calls, TotalMS: totalMS, MeanMS: totalMS / float64(calls),
		}}}
		seq := int64(360 * i) // 0.1/s background
		if i >= 3 {
			seq = 360*3 + 180000*int64(i-2) // 50/s from snapshot 3
		}
		size := int64(1000e6 + float64(i)*60e6) // ~18% growth over the window
		c.Tables = &model.Tables{Top: []model.TableStat{{
			Schema: "public", Name: "orders", SeqScans: seq, TotalBytes: size, LiveTuples: 1e6,
		}}}
	})
}

func TestAnalyze_flagshipSeqScanChain(t *testing.T) {
	r := Analyze(flagshipHistory(), nil, Options{})
	if len(r.Chains) == 0 {
		t.Fatal("expected the orders chain, got none")
	}
	ch := r.Chains[0]
	if !strings.Contains(ch.Symptom.Text, "42") && !strings.Contains(ch.Symptom.Text, "orders") {
		t.Errorf("symptom must name the query: %q", ch.Symptom.Text)
	}
	if !strings.Contains(ch.Symptom.Text, "3.2×") {
		t.Errorf("symptom must carry the slowdown factor 3.2×: %q", ch.Symptom.Text)
	}
	if len(ch.Hops) == 0 || !strings.Contains(ch.Hops[0].Text, "seq scan") {
		t.Fatalf("first hop must be the seq-scan mechanism: %+v", ch.Hops)
	}
	if !strings.Contains(ch.Hops[0].Text, "public.orders") {
		t.Errorf("mechanism must name the table: %q", ch.Hops[0].Text)
	}
	var growth bool
	for _, h := range ch.Hops[1:] {
		if strings.Contains(h.Text, "grew") {
			growth = true
		}
	}
	if !growth {
		t.Errorf("the 18%% growth antecedent must be attached: %+v", ch.Hops)
	}
	if ch.Confidence < 0.5 {
		t.Errorf("aligned chain with antecedent must clear 0.5, got %v", ch.Confidence)
	}
}

// Temporal discipline: a mechanism whose onset comes AFTER the symptom's
// cannot be its cause — the chain must not link them.
func TestAnalyze_mechanismAfterSymptomDoesNotChain(t *testing.T) {
	samples := history(6, func(i int, c *model.Context) {
		calls := int64(1000 * (i + 1))
		var totalMS float64
		if i < 2 { // symptom onset at snapshot 2
			totalMS = 8 * float64(calls)
		} else {
			totalMS = 8*2000 + 26*float64(calls-2000)
		}
		c.Queries = &model.Queries{Enabled: true, Top: []model.QueryStat{{
			QueryID: 42, Query: "SELECT * FROM orders WHERE customer_id = $1",
			Calls: calls, TotalMS: totalMS,
		}}}
		seq := int64(360 * i)
		if i >= 5 { // seq scans surge only at the last snapshot
			seq = 360*5 + 180000
		}
		c.Tables = &model.Tables{Top: []model.TableStat{{
			Schema: "public", Name: "orders", SeqScans: seq, TotalBytes: 1000e6, LiveTuples: 1e6,
		}}}
	})
	r := Analyze(samples, nil, Options{})
	for _, ch := range r.Chains {
		for _, h := range ch.Hops {
			if strings.Contains(h.Text, "seq scan") {
				t.Errorf("a later seq-scan onset must not be chained as cause: %+v", ch)
			}
		}
	}
}

// A surging table the query never references is not its mechanism.
func TestAnalyze_unreferencedTableDoesNotChain(t *testing.T) {
	samples := history(6, func(i int, c *model.Context) {
		calls := int64(1000 * (i + 1))
		var totalMS float64
		if i < 3 {
			totalMS = 8 * float64(calls)
		} else {
			totalMS = 8*3000 + 26*float64(calls-3000)
		}
		c.Queries = &model.Queries{Enabled: true, Top: []model.QueryStat{{
			QueryID: 42, Query: "SELECT * FROM invoices WHERE customer_id = $1",
			Calls: calls, TotalMS: totalMS,
		}}}
		seq := int64(360 * i)
		if i >= 3 {
			seq = 360*3 + 180000*int64(i-2)
		}
		c.Tables = &model.Tables{Top: []model.TableStat{{
			Schema: "public", Name: "orders", SeqScans: seq, TotalBytes: 1000e6, LiveTuples: 1e6,
		}}}
	})
	r := Analyze(samples, nil, Options{})
	for _, ch := range r.Chains {
		for _, h := range ch.Hops {
			if strings.Contains(h.Text, "seq scan") {
				t.Errorf("orders is not referenced by the query — no mechanism link: %+v", ch)
			}
		}
	}
}

// An index_dropped event on the table, occurring before the mechanism onset,
// is attached as the antecedent.
func TestAnalyze_indexDroppedAntecedent(t *testing.T) {
	samples := flagshipHistory()
	dropAt := samples[2].At.Add(-30 * time.Minute)
	events := []model.Event{{
		Kind: "schema.index_dropped", Object: "public.idx_orders_customer",
		OccurredBefore: &dropAt, Confidence: 1,
	}}
	r := Analyze(samples, events, Options{})
	if len(r.Chains) == 0 {
		t.Fatal("expected the orders chain")
	}
	var dropped bool
	for _, h := range r.Chains[0].Hops {
		if strings.Contains(h.Text, "idx_orders_customer") {
			dropped = true
		}
	}
	if !dropped {
		t.Errorf("index-dropped antecedent must be attached: %+v", r.Chains[0].Hops)
	}
}

// Fewer than MinSnapshots: no chains, and the report says exactly what to do.
func TestAnalyze_insufficientHistory(t *testing.T) {
	r := Analyze(flagshipHistory()[:2], nil, Options{})
	if len(r.Chains) != 0 {
		t.Errorf("2 snapshots cannot produce chains: %+v", r.Chains)
	}
	if len(r.Notes) == 0 || !strings.Contains(strings.Join(r.Notes, " "), "pgbot inspect") {
		t.Errorf("the report must tell the user to run pgbot inspect and return: %v", r.Notes)
	}
}

// A healthy, flat history yields no chains and no notes of concern.
func TestAnalyze_flatHistoryIsQuiet(t *testing.T) {
	samples := history(6, func(i int, c *model.Context) {
		calls := int64(1000 * (i + 1))
		c.Queries = &model.Queries{Enabled: true, Top: []model.QueryStat{{
			QueryID: 42, Query: "SELECT * FROM orders WHERE customer_id = $1",
			Calls: calls, TotalMS: 8 * float64(calls),
		}}}
		c.Tables = &model.Tables{Top: []model.TableStat{{
			Schema: "public", Name: "orders", SeqScans: int64(360 * i), TotalBytes: 1000e6, LiveTuples: 1e6,
		}}}
	})
	r := Analyze(samples, nil, Options{})
	if len(r.Chains) != 0 {
		t.Errorf("flat history must be quiet, got %+v", r.Chains)
	}
}

// The report must say what it analyzed and what it found BEFORE the cap, so
// the renderer can tell the user "showing 3 of 7" instead of a bare list.
func TestAnalyze_reportsScopeAndTotals(t *testing.T) {
	r := Analyze(flagshipHistory(), nil, Options{})
	if r.AnalyzedQueries != 1 || r.AnalyzedTables != 1 {
		t.Errorf("scope counts wrong: queries=%d tables=%d", r.AnalyzedQueries, r.AnalyzedTables)
	}
	if r.RegressionsFound != 1 {
		t.Errorf("RegressionsFound = %d, want 1", r.RegressionsFound)
	}
}

// MaxChains caps what is REPORTED, never what is counted: with a cap of 1 on a
// history holding two regressing queries, RegressionsFound stays 2.
func TestAnalyze_capReportsTotalFound(t *testing.T) {
	samples := history(6, func(i int, c *model.Context) {
		calls := int64(1000 * (i + 1))
		var totalMS float64
		if i < 3 {
			totalMS = 8 * float64(calls)
		} else {
			totalMS = 8*3000 + 26*float64(calls-3000)
		}
		c.Queries = &model.Queries{Enabled: true, TotalExecMS: 2 * totalMS, Top: []model.QueryStat{
			{QueryID: 42, Query: "SELECT * FROM orders WHERE customer_id = $1", Calls: calls, TotalMS: totalMS},
			{QueryID: 43, Query: "SELECT * FROM invoices WHERE due < $1", Calls: calls, TotalMS: totalMS},
		}}
	})
	r := Analyze(samples, nil, Options{MaxChains: 1})
	if len(r.Chains) != 1 {
		t.Fatalf("cap must limit chains to 1, got %d", len(r.Chains))
	}
	if r.RegressionsFound != 2 {
		t.Errorf("RegressionsFound = %d, want 2 (cap must not hide the total)", r.RegressionsFound)
	}
}

// Real-data regression: sub-millisecond means rendered "0ms → 0ms" on a 9.7×
// slowdown, and tiny per-second rates rendered "0.0 → 0.0". Small values keep
// significant digits; large ones stay terse.
func TestFormatters_smallValuesKeepPrecision(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0.041, "0.041ms"}, {0.4, "0.4ms"}, {4.2, "4.2ms"}, {26, "26ms"}, {1500, "1.5s"},
	} {
		if got := ms(tc.in); got != tc.want {
			t.Errorf("ms(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0.0042, "0.0042"}, {0.1, "0.1"}, {50, "50"},
	} {
		if got := rateStr(tc.in); got != tc.want {
			t.Errorf("rateStr(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
