package collect

import (
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
)

// A zero-scan index that backs ANY constraint (PK, unique, exclusion, or a FK's
// referenced key) must never be reported as unused — dropping it breaks the
// constraint. Verified through the collector's Unused filter (T9.3).
func TestIndexes_constraintBackedNeverUnused(t *testing.T) {
	rows := []indexRow{
		{Schema: "public", Table: "t", Index: "t_pkey", Scans: 0, Bytes: 1 << 20, IsPrimary: true, BacksConstraint: true},
		{Schema: "public", Table: "t", Index: "t_email_key", Scans: 0, Bytes: 1 << 20, IsUnique: true, BacksConstraint: true},
		{Schema: "public", Table: "t", Index: "t_no_overlap", Scans: 0, Bytes: 1 << 20, IsExclusion: true, BacksConstraint: true},
		{Schema: "public", Table: "t", Index: "t_fk_ref", Scans: 0, Bytes: 1 << 20, BacksConstraint: true}, // FK's referenced unique index
		{Schema: "public", Table: "t", Index: "t_plain_idx", Scans: 0, Bytes: 4 << 20},                     // the only genuinely droppable one
	}
	c := &model.Context{}
	indexesCollector{}.Assemble(c, conn.Capabilities{}, sampled{A: indexesSample{Rows: rows}}, 0, Options{})

	if c.Indexes == nil {
		t.Fatal("indexes section missing")
	}
	if len(c.Indexes.Unused) != 1 {
		t.Fatalf("only the plain index should be unused, got %d: %+v", len(c.Indexes.Unused), c.Indexes.Unused)
	}
	if c.Indexes.Unused[0].Name != "t_plain_idx" {
		t.Errorf("wrong index flagged unused: %s", c.Indexes.Unused[0].Name)
	}
}

func TestCockroachIndexesAgedUnusedSafetyAndRanking(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	old := now.Add(-14 * 24 * time.Hour)
	recent := now.Add(-2 * time.Hour)
	older := now.Add(-30 * 24 * time.Hour)
	rows := []cockroachIndexRow{
		{Database: "app", Schema: "public", Table: "orders", Index: "orders_pkey", IndexType: "primary", CreatedAt: &old, TotalIndexes: 6, SecondaryIndexes: 5},
		{Database: "app", Schema: "public", Table: "orders", Index: "orders_email_key", IndexType: "secondary", Unique: true, CreatedAt: &old, TotalWrites: 1000, TotalIndexes: 6, SecondaryIndexes: 5},
		{Database: "app", Schema: "public", Table: "orders", Index: "orders_old_idx", IndexType: "secondary", CreatedAt: &old, TotalWrites: 900, TotalIndexes: 6, SecondaryIndexes: 5},
		{Database: "app", Schema: "public", Table: "orders", Index: "orders_active_idx", IndexType: "secondary", CreatedAt: &old, LastRead: &recent, TotalReads: 40, TotalWrites: 80, TotalIndexes: 6, SecondaryIndexes: 5},
		{Database: "app", Schema: "public", Table: "orders", Index: "orders_unknown_age_idx", IndexType: "secondary", TotalIndexes: 6, SecondaryIndexes: 5},
		{Database: "app", Schema: "public", Table: "orders", Index: "orders_older_idx", IndexType: "secondary", CreatedAt: &older, TotalWrites: 2, TotalIndexes: 6, SecondaryIndexes: 5},
	}
	c := &model.Context{}
	caps := conn.Capabilities{Engine: conn.EngineCockroachDB, HasCRDBIndexUsage: true, HasCRDBIndexWrites: true}
	indexesCollector{}.Assemble(c, caps, sampled{A: cockroachIndexesSample{Rows: rows, CollectedAt: now, WritesAvailable: true}}, 0, Options{})

	if c.Indexes == nil || c.Indexes.Total != 6 || c.Indexes.SecondaryTotal != 5 {
		t.Fatalf("unexpected index summary: %+v", c.Indexes)
	}
	if c.Indexes.CountersDurable || !c.Indexes.WriteCountersAvailable {
		t.Fatalf("CockroachDB counter provenance missing: %+v", c.Indexes)
	}
	if got := indexNames(c.Indexes.Unused); strings.Join(got, ",") != "orders_older_idx,orders_old_idx" {
		t.Fatalf("unused candidates = %v; primary, unique, recent, and unknown-age indexes must be excluded", got)
	}
	if got := indexNames(c.Indexes.MostWritten); len(got) < 2 || got[0] != "orders_email_key" || got[1] != "orders_old_idx" {
		t.Fatalf("write ranking should use every secondary index: %v", got)
	}
}

func TestCockroachIndexQueriesAreBoundedAndVersionGated(t *testing.T) {
	for name, query := range map[string]string{"reads": sqlCockroachIndexes, "writes": sqlCockroachIndexesWrites} {
		if !strings.Contains(query, "LIMIT 500") || !strings.Contains(query, "current_database()") {
			t.Errorf("%s query must be database-scoped and bounded", name)
		}
	}
	if strings.Contains(sqlCockroachIndexes, "us.total_writes") || strings.Contains(sqlCockroachIndexes, "us.last_write") {
		t.Fatal("legacy query must not parse newer write-counter columns")
	}
	if !strings.Contains(sqlCockroachIndexesWrites, "us.total_writes") {
		t.Fatal("newer query should collect write counters")
	}
}

func indexNames(indexes []model.IndexStat) []string {
	out := make([]string, 0, len(indexes))
	for _, index := range indexes {
		out = append(out, index.Name)
	}
	return out
}

func TestIndexes_partialExpressionFlagsCarried(t *testing.T) {
	rows := []indexRow{
		{Schema: "public", Table: "t", Index: "p", Scans: 0, Bytes: 4 << 20, IsPartial: true},
		{Schema: "public", Table: "t", Index: "e", Scans: 0, Bytes: 4 << 20, IsExpression: true},
	}
	c := &model.Context{}
	indexesCollector{}.Assemble(c, conn.Capabilities{}, sampled{A: indexesSample{Rows: rows}}, 0, Options{})
	if len(c.Indexes.Unused) != 2 {
		t.Fatalf("both should be unused, got %d", len(c.Indexes.Unused))
	}
	if !c.Indexes.Unused[0].Partial || !c.Indexes.Unused[1].Expression {
		t.Errorf("partial/expression flags should carry to the finding, got %+v", c.Indexes.Unused)
	}
}
