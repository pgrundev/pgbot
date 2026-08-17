package collect

import (
	"context"
	_ "embed"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
)

//go:embed sql/indexes.sql
var sqlIndexes string

//go:embed sql/redundant_indexes.sql
var sqlRedundantIndexes string

//go:embed sql/unindexed_fks.sql
var sqlUnindexedFKs string

// indexes = pg_stat_user_indexes. Zero-scan, non-trivial indexes that don't
// back a primary key or unique constraint become the unused-index finding.
type indexesCollector struct{}

type indexRow struct {
	Schema          string `db:"schema"`
	Table           string `db:"table"`
	Index           string `db:"index"`
	Scans           int64  `db:"scans"`
	Bytes           int64  `db:"bytes"`
	Definition      string `db:"definition"`
	IsPrimary       bool   `db:"is_primary"`
	IsUnique        bool   `db:"is_unique"`
	IsExclusion     bool   `db:"is_exclusion"`
	IsPartial       bool   `db:"is_partial"`
	IsExpression    bool   `db:"is_expression"`
	BacksConstraint bool   `db:"backs_constraint"`
}

func (indexesCollector) Name() string                     { return "indexes" }
func (indexesCollector) Kind() Kind                       { return KindGauge }
func (indexesCollector) Available(conn.Capabilities) bool { return true }

type redundantRow struct {
	Schema    string `db:"schema"`
	Table     string `db:"table"`
	Redundant string `db:"redundant_index"`
	Covering  string `db:"covering_index"`
	Bytes     int64  `db:"redundant_bytes"`
}

type fkRow struct {
	Schema     string `db:"schema"`
	Table      string `db:"child_table"`
	Constraint string `db:"constraint_name"`
	ChildBytes int64  `db:"child_bytes"`
	FKColumns  string `db:"fk_columns"`
}

type indexesSample struct {
	Rows      []indexRow
	Redundant []redundantRow
	FKs       []fkRow
	Total     int // count of ALL user indexes, not just the scanned window
}

func (indexesCollector) Sample(ctx context.Context, t *conn.Target, _ conn.Capabilities) (any, error) {
	rows, err := queryMany[indexRow](ctx, t, sqlIndexes)
	if err != nil {
		return nil, err
	}
	out := indexesSample{Rows: rows}
	// indexes.sql is LIMIT 200 (the largest, for the list). Total is the real
	// user-index count so the report says "398 total" not "200 total"; anything
	// unused below the 200-largest cut is small and out of scope for the finding.
	out.Total, _ = scalar[int](ctx, t, `SELECT count(*)::int FROM pg_stat_user_indexes`)
	// Redundant-index and unindexed-FK detection are pure catalog reads; a failure
	// in either must not sink the whole indexes section (it degrades to no list).
	out.Redundant, _ = queryMany[redundantRow](ctx, t, sqlRedundantIndexes)
	out.FKs, _ = queryMany[fkRow](ctx, t, sqlUnindexedFKs)
	return out, nil
}

func (indexesCollector) Assemble(c *model.Context, _ conn.Capabilities, s sampled, _ time.Duration, _ Options) {
	sm, ok := s.A.(indexesSample)
	if s.Err != nil || !ok {
		c.Indexes = &model.Indexes{Section: unavail(s.Err, "pg_stat_user_indexes unavailable")}
		return
	}
	rows := sm.Rows
	total := sm.Total
	if total == 0 {
		total = len(rows) // count query failed — fall back to what we scanned
	}
	idx := &model.Indexes{Section: model.Section{Exactness: model.ExactnessScraped}, Total: total, Scanned: len(rows)}
	for _, r := range sm.Redundant {
		idx.Redundant = append(idx.Redundant, model.RedundantIndex{
			Schema: r.Schema, Table: r.Table, Name: r.Redundant, CoveredBy: r.Covering, Bytes: r.Bytes,
		})
	}
	for _, r := range sm.FKs {
		idx.UnindexedFKs = append(idx.UnindexedFKs, model.UnindexedFK{
			Schema: r.Schema, Table: r.Table, Constraint: r.Constraint, Columns: r.FKColumns, ChildBytes: r.ChildBytes,
		})
	}
	for _, r := range rows {
		st := model.IndexStat{
			Schema: r.Schema, Table: r.Table, Name: r.Index, Scans: r.Scans, Bytes: r.Bytes,
			Definition: r.Definition, Partial: r.IsPartial, Expression: r.IsExpression,
		}
		// An index that enforces ANY constraint (PK, UNIQUE, EXCLUSION, or a FK's
		// referenced key) can't be dropped, so it is never "unused" no matter its
		// scan count. backs_constraint (pg_constraint.conindid) covers all of them;
		// is_primary/is_unique/is_exclusion are belt-and-suspenders.
		constraintBacked := r.BacksConstraint || r.IsPrimary || r.IsUnique || r.IsExclusion
		if r.Scans == 0 && r.Bytes > 16384 && !constraintBacked && len(idx.Unused) < 50 {
			idx.Unused = append(idx.Unused, st)
		}
		if len(idx.Largest) < 10 {
			idx.Largest = append(idx.Largest, st) // rows arrive largest-first
		}
	}
	c.Indexes = idx
}
