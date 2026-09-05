package report

import (
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/model"
)

func f64(v float64) *float64 { return &v }

func fixtureContext() *model.Context {
	return &model.Context{
		SchemaVersion: "1.2.0",
		Fingerprint:   "abc123",
		Server: model.ServerInfo{
			VersionNum: 170004, VersionText: "PostgreSQL 17.4", Database: "shop", Provider: "rds",
		},
		Health: &model.Health{Connections: 42, TPS: f64(85.7), CacheHitRatio: f64(0.994)},
		Findings: []model.Finding{
			{ID: "unused_indexes", Severity: "warning", Title: "3 unused indexes consume 18 GB",
				Detail: "these indexes have zero scans", Remediation: "review before dropping",
				Caveats: []string{"scan counts are per-node; a replica may still use them"}},
			{ID: "xid_age", Severity: "critical", Title: "transaction-id age approaching wraparound",
				Detail: "84% toward the 2B ceiling"},
		},
		Queries: &model.Queries{Enabled: true, TotalExecMS: 1000, Top: []model.QueryStat{
			{QueryID: 42, Query: "SELECT * FROM orders WHERE user_id = $1", Calls: 812400, TotalMS: 610.9, MeanMS: 18.55},
		}},
		Tables: &model.Tables{DBSizeBytes: 91887295, Top: []model.TableStat{
			{Schema: "public", Name: "orders", TotalBytes: 38617088, LiveTuples: 460, DeadRatio: 0.44, SeqScans: 9},
		}},
		Indexes: &model.Indexes{Total: 154, Unused: []model.IndexStat{
			{Schema: "public", Table: "orders", Name: "idx_orders_legacy", Scans: 0, Bytes: 1818624,
				Definition: "CREATE INDEX idx_orders_legacy ON public.orders (status)"},
		}},
		Settings: &model.Settings{Overrides: map[string]string{"shared_buffers": "4GB"}},
	}
}

// The report is one self-contained page carrying every section the Context
// holds: findings with severity and caveats, queries, tables, indexes,
// settings — plus the health score and DB header.
func TestRenderReport(t *testing.T) {
	out := Render(fixtureContext(), 82, "0.7.1")

	for _, want := range []string{
		"</html>", "shop", "PostgreSQL 17.4", "82",
		"transaction-id age approaching wraparound", // critical finding
		"3 unused indexes consume 18 GB",
		"scan counts are per-node", // caveats render inline, never dropped
		"SELECT * FROM orders WHERE user_id = $1",
		"idx_orders_legacy", "public.orders",
		"shared_buffers", "4GB",
		"87.6 MiB", // db size humanized
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q", want)
		}
	}
	// Self-contained: pinned like erd --html.
	for _, banned := range []string{"https://", "http://", "src=", "@import"} {
		if strings.Contains(out, banned) {
			t.Errorf("report must be self-contained, found %q", banned)
		}
	}
	if out != Render(fixtureContext(), 82, "0.7.1") {
		t.Error("report must be deterministic")
	}
	// Escaping: query text with markup-hostile characters must not break out.
	c := fixtureContext()
	c.Queries.Top[0].Query = `SELECT '<script>' FROM t WHERE a < 2`
	if s := Render(c, 82, "x"); strings.Contains(s, "<script>' FROM") {
		t.Error("query text must be escaped")
	}
}

// Sections the Context doesn't carry are skipped without a trace of "nil".
func TestRenderReportSparse(t *testing.T) {
	c := &model.Context{Server: model.ServerInfo{Database: "empty", VersionNum: 160000}}
	out := Render(c, 100, "x")
	if strings.Contains(out, "nil") || strings.Contains(out, "%!") {
		t.Errorf("sparse context must render clean: %s", out)
	}
	if !strings.Contains(out, "empty") {
		t.Error("header must still carry the database")
	}
}
