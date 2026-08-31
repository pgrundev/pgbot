package collect

import (
	"context"
	_ "embed"
	"sort"
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

//go:embed sql/cockroach_indexes.sql
var sqlCockroachIndexes string

//go:embed sql/cockroach_indexes_writes.sql
var sqlCockroachIndexesWrites string

const (
	cockroachIndexStatsSource     = "cockroachdb_cluster_index_usage"
	cockroachUnusedIndexThreshold = 7 * 24 * time.Hour
)

// indexes = pg_stat_user_indexes. Zero-scan, non-trivial indexes that don't
// back a primary key or unique constraint become the unused-index finding.
type indexesCollector struct{}

type indexRow struct {
	Schema          string   `db:"schema"`
	Table           string   `db:"table"`
	Index           string   `db:"index"`
	Scans           int64    `db:"scans"`
	Bytes           int64    `db:"bytes"`
	Definition      string   `db:"definition"`
	Method          string   `db:"method"`
	IsPrimary       bool     `db:"is_primary"`
	IsUnique        bool     `db:"is_unique"`
	IsExclusion     bool     `db:"is_exclusion"`
	IsReplident     bool     `db:"is_replident"`
	IsPartial       bool     `db:"is_partial"`
	IsExpression    bool     `db:"is_expression"`
	BacksConstraint bool     `db:"backs_constraint"`
	Columns         []string `db:"columns"`
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

type cockroachIndexRow struct {
	Database         string     `db:"database_name"`
	Schema           string     `db:"schema_name"`
	Table            string     `db:"table_name"`
	Index            string     `db:"index_name"`
	IndexType        string     `db:"index_type"`
	Unique           bool       `db:"is_unique"`
	Inverted         bool       `db:"is_inverted"`
	Sharded          bool       `db:"is_sharded"`
	Visible          bool       `db:"is_visible"`
	CreatedAt        *time.Time `db:"created_at"`
	TotalReads       int64      `db:"total_reads"`
	LastRead         *time.Time `db:"last_read"`
	TotalWrites      int64      `db:"total_writes"`
	LastWrite        *time.Time `db:"last_write"`
	TotalIndexes     int64      `db:"total_indexes"`
	SecondaryIndexes int64      `db:"secondary_indexes"`
}

type cockroachIndexesSample struct {
	Rows            []cockroachIndexRow
	CollectedAt     time.Time
	WritesAvailable bool
}

func (indexesCollector) Sample(ctx context.Context, t *conn.Target, caps conn.Capabilities) (any, error) {
	if caps.IsCockroachDB() {
		if !caps.HasCRDBIndexUsage {
			return cockroachIndexesSample{}, nil
		}
		query := sqlCockroachIndexes
		if caps.HasCRDBIndexWrites {
			query = sqlCockroachIndexesWrites
		}
		rows, err := queryMany[cockroachIndexRow](ctx, t, query)
		return cockroachIndexesSample{Rows: rows, CollectedAt: time.Now().UTC(), WritesAvailable: caps.HasCRDBIndexWrites}, err
	}
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

func (indexesCollector) Assemble(c *model.Context, caps conn.Capabilities, s sampled, _ time.Duration, _ Options) {
	if caps.IsCockroachDB() {
		assembleCockroachIndexes(c, caps, s)
		return
	}
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
	idx := &model.Indexes{
		Section: model.Section{Exactness: model.ExactnessScraped}, Total: total, Scanned: len(rows),
		StatsSource: "pg_stat_user_indexes", CountersDurable: true,
	}
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
			Definition: r.Definition, Columns: r.Columns, Method: r.Method,
			Unique: r.IsUnique, Primary: r.IsPrimary, Partial: r.IsPartial, Expression: r.IsExpression,
		}
		// An index that enforces ANY constraint (PK, UNIQUE, EXCLUSION, or a FK's
		// referenced key), or that is the table's REPLICA IDENTITY, can't be dropped,
		// so it is never "unused" no matter its scan count. backs_constraint
		// (pg_constraint.conindid) covers the constraints; is_primary/is_unique/
		// is_exclusion are belt-and-suspenders; is_replident guards logical
		// replication + UPDATE/DELETE row identity (a replica-identity index shows
		// zero scans on the primary but dropping it breaks replication).
		constraintBacked := r.BacksConstraint || r.IsPrimary || r.IsUnique || r.IsExclusion || r.IsReplident
		if r.Scans == 0 && r.Bytes > 16384 && !constraintBacked && len(idx.Unused) < 50 {
			idx.Unused = append(idx.Unused, st)
		}
		if len(idx.Largest) < 10 {
			idx.Largest = append(idx.Largest, st) // rows arrive largest-first
		}
	}
	c.Indexes = idx
}

func assembleCockroachIndexes(c *model.Context, caps conn.Capabilities, s sampled) {
	if !caps.HasCRDBIndexUsage {
		c.Indexes = &model.Indexes{Section: unavail(nil, "CockroachDB cluster index usage is not available on this version")}
		return
	}
	sm, ok := s.A.(cockroachIndexesSample)
	if s.Err != nil || !ok {
		c.Indexes = &model.Indexes{Section: unavail(s.Err, "CockroachDB cluster index usage read failed")}
		return
	}
	idx := &model.Indexes{
		Section:                model.Section{Exactness: model.ExactnessScraped},
		Scanned:                len(sm.Rows),
		StatsSource:            cockroachIndexStatsSource,
		CountersDurable:        false,
		WriteCountersAvailable: sm.WritesAvailable,
		UnusedThresholdHours:   int(cockroachUnusedIndexThreshold / time.Hour),
	}
	if len(sm.Rows) > 0 {
		idx.Total = int(sm.Rows[0].TotalIndexes)
		idx.SecondaryTotal = int(sm.Rows[0].SecondaryIndexes)
	}
	for _, r := range sm.Rows {
		st := model.IndexStat{
			Database: r.Database, Schema: r.Schema, Table: r.Table, Name: r.Index,
			Scans: r.TotalReads, Writes: r.TotalWrites, LastRead: utcTime(r.LastRead), LastWrite: utcTime(r.LastWrite), CreatedAt: utcTime(r.CreatedAt),
			IndexType: r.IndexType, Unique: r.Unique, Primary: r.IndexType == "primary", Inverted: r.Inverted,
			Sharded: r.Sharded, Invisible: !r.Visible,
		}
		if r.IndexType != "secondary" {
			continue
		}
		if sm.WritesAvailable {
			idx.MostWritten = append(idx.MostWritten, st)
		}
		if unusedFor, candidate := cockroachIndexUnusedFor(r, sm.CollectedAt); candidate {
			st.UnusedForSeconds = unusedFor.Seconds()
			if len(idx.Unused) < 50 {
				idx.Unused = append(idx.Unused, st)
			}
		}
		idx.Usage = append(idx.Usage, st)
	}
	sort.SliceStable(idx.Usage, func(i, j int) bool {
		if idx.Usage[i].Scans != idx.Usage[j].Scans {
			return idx.Usage[i].Scans > idx.Usage[j].Scans
		}
		return indexObject(idx.Usage[i]) < indexObject(idx.Usage[j])
	})
	if len(idx.Usage) > 20 {
		idx.Usage = idx.Usage[:20]
	}
	sort.SliceStable(idx.Unused, func(i, j int) bool {
		if idx.Unused[i].UnusedForSeconds != idx.Unused[j].UnusedForSeconds {
			return idx.Unused[i].UnusedForSeconds > idx.Unused[j].UnusedForSeconds
		}
		return indexObject(idx.Unused[i]) < indexObject(idx.Unused[j])
	})
	if sm.WritesAvailable {
		sort.SliceStable(idx.MostWritten, func(i, j int) bool {
			if idx.MostWritten[i].Writes != idx.MostWritten[j].Writes {
				return idx.MostWritten[i].Writes > idx.MostWritten[j].Writes
			}
			return indexObject(idx.MostWritten[i]) < indexObject(idx.MostWritten[j])
		})
		if len(idx.MostWritten) > 10 {
			idx.MostWritten = idx.MostWritten[:10]
		}
	}
	c.Indexes = idx
}

func cockroachIndexUnusedFor(r cockroachIndexRow, collectedAt time.Time) (time.Duration, bool) {
	if r.IndexType != "secondary" || r.Unique {
		return 0, false
	}
	lastActive := r.LastRead
	if lastActive == nil {
		lastActive = r.CreatedAt
	}
	// Missing creation time plus no recorded read does not establish a safe
	// observation window. Keep it visible in usage, but never call it unused.
	if lastActive == nil || collectedAt.Before(*lastActive) {
		return 0, false
	}
	unusedFor := collectedAt.Sub(*lastActive)
	return unusedFor, unusedFor >= cockroachUnusedIndexThreshold
}

func indexObject(ix model.IndexStat) string {
	return ix.Database + "." + ix.Schema + "." + ix.Table + "/" + ix.Name
}
