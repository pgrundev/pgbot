package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/store"
)

// whyFixtureStore writes the flagship regression history into a temp store:
// query 42 on orders slows 8ms → 26ms per call at snapshot 3, seq scans surge
// with it, the table grows ~18% across the window.
func whyFixtureStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "baselines.db")
	writeWhyHistory(t, path)
	return path
}

// seedWhyFixture writes the same history into the DEFAULT store location —
// for tools that resolve the store themselves (set XDG_STATE_HOME first).
func seedWhyFixture(t *testing.T) {
	t.Helper()
	writeWhyHistory(t, "")
}

func writeWhyHistory(t *testing.T, path string) {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Now().UTC().Add(-6 * time.Hour)
	for i := 0; i < 6; i++ {
		calls := int64(1000 * (i + 1))
		var totalMS float64
		if i < 3 {
			totalMS = 8 * float64(calls)
		} else {
			totalMS = 8*3000 + 26*float64(calls-3000)
		}
		seq := int64(360 * i)
		if i >= 3 {
			seq = 360*3 + 180000*int64(i-2)
		}
		c := &model.Context{
			Fingerprint:   "whyfixture",
			CollectedAt:   t0.Add(time.Duration(i) * time.Hour),
			SchemaVersion: model.SchemaVersion,
			Server:        model.ServerInfo{Database: "app"},
			Queries: &model.Queries{Enabled: true, TotalExecMS: totalMS, Top: []model.QueryStat{{
				QueryID: 42, Query: "SELECT * FROM orders WHERE customer_id = $1",
				Calls: calls, TotalMS: totalMS,
			}}},
			Tables: &model.Tables{Top: []model.TableStat{{
				Schema: "public", Name: "orders",
				SeqScans: seq, TotalBytes: int64(1000e6 + float64(i)*60e6), LiveTuples: 1e6,
			}}},
		}
		if _, err := st.Save(c); err != nil {
			t.Fatal(err)
		}
	}
}

func runWhyCmd(t *testing.T, args ...string) string {
	t.Helper()
	cmd := newWhyCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("why failed: %v\n%s", err, buf.String())
	}
	return buf.String()
}

// The flagship narrative, end to end from a stored history: symptom with the
// factor, indented because/after chain, and hedged confidence wording.
func TestWhy_narrative(t *testing.T) {
	out := runWhyCmd(t, "--store", whyFixtureStore(t), "--no-color")
	for _, want := range []string{"slowed 3.2×", "because seq scans", "public.orders", "grew"} {
		if !strings.Contains(out, want) {
			t.Errorf("narrative missing %q:\n%s", want, out)
		}
	}
}

// --json emits the versioned Report shape.
func TestWhy_json(t *testing.T) {
	out := runWhyCmd(t, "--store", whyFixtureStore(t), "--json")
	var r struct {
		SchemaVersion string `json:"why_schema_version"`
		Database      string `json:"database"`
		Chains        []struct {
			Symptom    struct{ Text string }
			Confidence float64
		} `json:"chains"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if r.SchemaVersion != "1.1.0" || r.Database != "app" || len(r.Chains) == 0 {
		t.Errorf("unexpected report: %+v", r)
	}
}

// An empty store gets the setup message, not an error.
func TestWhy_emptyStore(t *testing.T) {
	cmd := newWhyCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--store", filepath.Join(t.TempDir(), "empty.db")})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "pgbot inspect") {
		t.Errorf("empty store must say to run pgbot inspect first, got err=%v out=%s", err, buf.String())
	}
}

// The narrative must explain itself to someone who has never seen it: what
// was analyzed, how many regressions were found vs shown, and how to read a
// chain — so the output carries its own context.
func TestWhy_selfDescribingOutput(t *testing.T) {
	out := runWhyCmd(t, "--store", whyFixtureStore(t), "--no-color")
	for _, want := range []string{
		"analyzed 1 quer",        // scope: what was looked at
		"1 sustained regression", // total found
		"how to read this",       // the legend line
		"what regressed \u2190 the mechanism \u2190 what set it off", // chain anatomy
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// When the cap hides chains, the output says so and names the flag.
func TestWhy_capIsAnnounced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baselines.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC().Add(-6 * time.Hour)
	for i := 0; i < 6; i++ {
		calls := int64(1000 * (i + 1))
		var totalMS float64
		if i < 3 {
			totalMS = 8 * float64(calls)
		} else {
			totalMS = 8*3000 + 26*float64(calls-3000)
		}
		top := make([]model.QueryStat, 0, 7)
		for q := int64(0); q < 7; q++ { // seven regressing queries, cap default 5
			top = append(top, model.QueryStat{
				QueryID: 100 + q, Query: "SELECT * FROM t" + string(rune('a'+q)) + " WHERE id = $1",
				Calls: calls, TotalMS: totalMS,
			})
		}
		c := &model.Context{
			Fingerprint: "capfix", CollectedAt: t0.Add(time.Duration(i) * time.Hour),
			SchemaVersion: model.SchemaVersion,
			Server:        model.ServerInfo{Database: "app"},
			Queries:       &model.Queries{Enabled: true, TotalExecMS: 4 * totalMS, Top: top},
		}
		if _, err := st.Save(c); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	out := runWhyCmd(t, "--store", path, "--no-color")
	if !strings.Contains(out, "showing the 5 worst of 7") || !strings.Contains(out, "--max-chains") {
		t.Errorf("capped output must say showing-5-of-7 and name --max-chains:\n%s", out)
	}
	all := runWhyCmd(t, "--store", path, "--no-color", "--max-chains", "10")
	if c := strings.Count(all, "\u25cf "); c != 7 {
		t.Errorf("--max-chains 10 must show all 7 chains, got %d:\n%s", c, all)
	}
	// `pgbot why N` — the positional count is the ergonomic form of --max-chains.
	two := runWhyCmd(t, "--store", path, "--no-color", "2")
	if c := strings.Count(two, "\u25cf "); c != 2 {
		t.Errorf("`why 2` must show exactly 2 chains, got %d:\n%s", c, two)
	}
	if !strings.Contains(two, "showing the 2 worst of 7") {
		t.Errorf("`why 2` must still announce the total:\n%s", two)
	}
}

// "Only 2 snapshots" while the store visibly holds 26 reads as a bug. When
// history exists OUTSIDE the window, the guidance must say so and name
// --window, not just ask for more inspect runs.
func TestWhy_windowTruncationIsExplained(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baselines.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	save := func(age time.Duration) {
		c := &model.Context{
			Fingerprint: "trunc", CollectedAt: now.Add(-age), SchemaVersion: model.SchemaVersion,
			Server: model.ServerInfo{Database: "app"},
		}
		if _, err := st.Save(c); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 24; i++ { // plenty of history, all older than the window
		save(time.Duration(10+i) * 24 * time.Hour)
	}
	save(5 * 24 * time.Hour) // only two inside the default 7 days
	save(4 * 24 * time.Hour)
	st.Close()

	out := runWhyCmd(t, "--store", path, "--no-color")
	if !strings.Contains(out, "24 more snapshot") || !strings.Contains(out, "--window") {
		t.Errorf("window truncation must be named with the --window fix:\n%s", out)
	}
}

// `pgbot why 10` (chain count) must keep working while `why --duration 10s
// <dsn>` accepts a connection string in the same positional slot.
func TestWhyArgIsDSN(t *testing.T) {
	for _, dsn := range []string{"postgres://u@h/db", "postgresql://h/db", "host=h dbname=d"} {
		if !whyArgIsDSN(dsn) {
			t.Errorf("%q must read as a DSN", dsn)
		}
	}
	for _, n := range []string{"10", "3"} {
		if whyArgIsDSN(n) {
			t.Errorf("%q must read as a chain count", n)
		}
	}
}
