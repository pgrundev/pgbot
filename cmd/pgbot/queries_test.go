package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/model"
)

func TestDurFromMs(t *testing.T) {
	cases := []struct {
		ms   float64
		want string
	}{
		{0, "0ms"},
		{45, "45ms"},
		{810.79, "811ms"},
		{1200, "1.2s"},
		{59_000, "59.0s"},
		{90_000, "1m30s"},
		{3_600_000, "1h0m"},
		{5_400_000, "1h30m"},
		{90_000_000, "1d1h"},     // 25 hours
		{3 * 86_400_000, "3d0h"}, // 3 days
	}
	for _, c := range cases {
		if got := durFromMs(c.ms); got != c.want {
			t.Errorf("durFromMs(%g) = %q, want %q", c.ms, got, c.want)
		}
	}
}

func TestHumanCount(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{60, "60"},
		{999, "999"},
		{1000, "1.0k"},
		{812394, "812.4k"},
		{4_821_004, "4.8M"},
		{3_100_000_000, "3.1B"},
	}
	for _, c := range cases {
		if got := humanCount(c.n); got != c.want {
			t.Errorf("humanCount(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestTruncStr(t *testing.T) {
	if got := truncStr("short", 60); got != "short" {
		t.Errorf("short string should pass through, got %q", got)
	}
	long := "SELECT * FROM a_very_long_table_name_that_exceeds_the_limit_easily"
	got := truncStr(long, 20)
	if len([]rune(got)) != 20 {
		t.Errorf("truncated length = %d runes, want 20 (%q)", len([]rune(got)), got)
	}
	if got[len(got)-len("…"):] != "…" {
		t.Errorf("truncated string should end with ellipsis, got %q", got)
	}
}

func TestQueryConsumers_distinguishDisabledEmptyAndUnavailable(t *testing.T) {
	states := []struct {
		name          string
		queries       *model.Queries
		cliContains   []string
		cliExcludes   []string
		wantEnabled   bool
		wantAvailable bool
		wantReason    string
	}{
		{
			name:        "missing query section",
			cliContains: []string{"pg_stat_statements is required for query stats"},
			cliExcludes: []string{"top 0 queries"},
		},
		{
			name: "extension disabled",
			queries: &model.Queries{Enabled: false, Section: model.Section{
				Exactness: model.ExactnessUnavailable,
				Reason:    "pg_stat_statements not enabled — install the extension",
			}},
			cliContains:   []string{"pg_stat_statements is required for query stats", "install the extension"},
			cliExcludes:   []string{"top 0 queries"},
			wantEnabled:   false,
			wantAvailable: false,
			wantReason:    "pg_stat_statements not enabled — install the extension",
		},
		{
			name: "enabled empty workload",
			queries: &model.Queries{Enabled: true, Section: model.Section{
				Exactness: model.ExactnessCumulative,
			}},
			cliContains:   []string{"top 0 queries", "by total execution time"},
			cliExcludes:   []string{"unavailable", "is required"},
			wantEnabled:   true,
			wantAvailable: true,
		},
		{
			name: "enabled unavailable section",
			queries: &model.Queries{Enabled: true, Section: model.Section{
				Exactness: model.ExactnessUnavailable,
				Reason:    "permission denied while using postgres://monitor:secret@db.internal/app",
			}},
			cliContains:   []string{"Query stats are unavailable", "pg_stat_statements read failed"},
			cliExcludes:   []string{"top 0 queries", "secret", "db.internal"},
			wantEnabled:   true,
			wantAvailable: false,
			wantReason:    "pg_stat_statements read failed",
		},
	}

	for _, tc := range states {
		t.Run(tc.name, func(t *testing.T) {
			c := &model.Context{
				Server:  model.ServerInfo{VersionNum: 160000, Database: "app"},
				Queries: tc.queries,
			}
			useQueryContext(t, c)

			cli := executeQueriesCommand(t)
			for _, want := range tc.cliContains {
				if !strings.Contains(cli, want) {
					t.Errorf("CLI output missing %q:\n%s", want, cli)
				}
			}
			for _, unwanted := range tc.cliExcludes {
				if strings.Contains(cli, unwanted) {
					t.Errorf("CLI output unexpectedly contains %q:\n%s", unwanted, cli)
				}
			}

			out, err := topQueriesTool(context.Background(), json.RawMessage(`{"connection_string":"postgres://fixture/app"}`))
			if err != nil {
				t.Fatalf("top_queries: %v", err)
			}
			var response struct {
				Enabled   bool   `json:"enabled"`
				Available *bool  `json:"available"`
				Reason    string `json:"reason"`
				Queries   []any  `json:"queries"`
			}
			if err := json.Unmarshal([]byte(out), &response); err != nil {
				t.Fatalf("top_queries returned invalid JSON: %v\n%s", err, out)
			}
			if response.Enabled != tc.wantEnabled {
				t.Errorf("MCP enabled = %v, want %v: %s", response.Enabled, tc.wantEnabled, out)
			}
			if response.Available == nil || *response.Available != tc.wantAvailable {
				t.Errorf("MCP available = %v, want %v: %s", response.Available, tc.wantAvailable, out)
			}
			if response.Reason != tc.wantReason {
				t.Errorf("MCP reason = %q, want %q: %s", response.Reason, tc.wantReason, out)
			}
			if len(response.Queries) != 0 {
				t.Errorf("fixture has no query rows: %s", out)
			}
			if strings.Contains(out, "secret") || strings.Contains(out, "db.internal") {
				t.Errorf("MCP response exposed the collector error without output sanitization: %s", out)
			}
		})
	}
}

func useQueryContext(t *testing.T, c *model.Context) {
	t.Helper()
	previous := gatherQueryContext
	gatherQueryContext = func(context.Context, string, inspectFlags) (*model.Context, string, error) {
		return c, "db.example", nil
	}
	t.Cleanup(func() { gatherQueryContext = previous })
}

func executeQueriesCommand(t *testing.T) string {
	t.Helper()
	cmd := newQueriesCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"postgres://fixture/app", "--no-color"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("pgbot queries: %v\n%s", err, out.String())
	}
	return out.String()
}

func TestQueryConsumers_keepPopulatedResultsAndGatherErrors(t *testing.T) {
	c := &model.Context{Server: model.ServerInfo{VersionNum: 160000, Database: "app"}, Queries: &model.Queries{Enabled: true, Section: model.Section{Exactness: model.ExactnessCumulative}, TotalExecMS: 100, Top: []model.QueryStat{{QueryID: 1, Query: "SELECT total_first", Calls: 1, TotalMS: 90, MeanMS: 90}, {QueryID: 2, Query: "SELECT calls_first", Calls: 10, TotalMS: 10, MeanMS: 1}}}}
	useQueryContext(t, c)
	for _, byCalls := range []bool{false, true} {
		cmd := newQueriesCmd()
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		args := []string{"postgres://fixture/app", "--no-color"}
		if byCalls {
			args = append(args, "--by-calls")
		}
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		got := output.String()
		first, second := "total_first", "calls_first"
		if byCalls {
			first, second = second, first
		}
		if !strings.Contains(got, first) || !strings.Contains(got, second) || strings.Index(got, first) > strings.Index(got, second) {
			t.Fatalf("incorrect query ordering: %s", got)
		}
	}
	if c.Queries.Top[0].QueryID != 1 {
		t.Fatal("CLI ranking mutated collected query order")
	}
	text, err := topQueriesTool(context.Background(), json.RawMessage(`{"connection_string":"postgres://fixture/app"}`))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Available bool    `json:"available"`
		Total     float64 `json:"total_exec_ms"`
		Queries   []struct {
			Share float64 `json:"share_pct"`
		} `json:"queries"`
	}
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Available || result.Total != 100 || len(result.Queries) != 2 || result.Queries[0].Share != 90 {
		t.Fatalf("populated MCP contract changed: %s", text)
	}
	sentinel := errors.New("synthetic collection failure")
	gatherQueryContext = func(context.Context, string, inspectFlags) (*model.Context, string, error) { return nil, "", sentinel }
	cmd := newQueriesCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"postgres://fixture/app", "--no-color"})
	if err := cmd.Execute(); !errors.Is(err, sentinel) {
		t.Fatalf("CLI gather error=%v", err)
	}
	if _, err := topQueriesTool(context.Background(), json.RawMessage(`{"connection_string":"postgres://fixture/app"}`)); !errors.Is(err, sentinel) {
		t.Fatalf("MCP gather error=%v", err)
	}
}
