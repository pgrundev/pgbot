package collect

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/rate"
)

//go:embed sql/queries.sql
var sqlQueries string

// queries = top of pg_stat_statements, cumulative since stats reset. The
// temporal view comes from the baseline diff, not a short in-process sample, so
// this is a single read (KindGauge) even though the columns are counters.
type queriesCollector struct{}

type queryRow struct {
	QueryID        int64   `db:"queryid"`
	Query          string  `db:"query"`
	Calls          int64   `db:"calls"`
	TotalMS        float64 `db:"total_ms"`
	MeanMS         float64 `db:"mean_ms"`
	MaxMS          float64 `db:"max_ms"`
	Rows           int64   `db:"rows"`
	SharedBlksHit  int64   `db:"shared_blks_hit"`
	SharedBlksRead int64   `db:"shared_blks_read"`
	WalBytes       int64   `db:"wal_bytes"`
	TotalExecAll   float64 `db:"total_exec_all"`
}

func (queriesCollector) Name() string { return "queries" }
func (queriesCollector) Kind() Kind   { return KindGauge }
func (queriesCollector) Available(caps conn.Capabilities) bool {
	return caps.HasStatStatements
}

// queriesSample bundles the top-N rows with pg_stat_statements saturation health
// (A4): whether it's evicting entries, which biases the top list and can reset a
// query's stats mid-comparison.
type queriesSample struct {
	Rows    []queryRow
	Dealloc int64
	Count   int
	Max     int
}

func (queriesCollector) Sample(ctx context.Context, t *conn.Target, caps conn.Capabilities) (any, error) {
	// The version-appropriate total-time column comes from a fixed allowlist
	// (never user input); %% in the SQL escapes the ILIKE literal.
	//
	// pg_stat_statements is a set-returning function: every row (with up to
	// 4 KB of query text) is materialized in a tuplestore before ORDER BY/LIMIT,
	// and the window sum() OVER () keeps all of them. At the default
	// work_mem=4MB and ~5k tracked statements that is ~5 MB of temp file per
	// pgbot run — landing inside pgbot's own sample window, where the health
	// collector reads it back as temp_bytes_per_sec ≈ 1 MiB/s and the tune rule
	// recommends raising work_mem for it. A transaction-local work_mem keeps
	// this read in memory (measured: temp written=612 blocks → 0).
	rows, err := queryManyLocal[queryRow](ctx, t, []string{"SET LOCAL work_mem = '64MB'"},
		fmt.Sprintf(sqlQueries, caps.StatStatementsTotalCol()))
	if err != nil {
		return nil, err
	}
	out := queriesSample{Rows: rows}
	// showtext := false — counting through the view materializes every row WITH
	// its query text (the same ~5 MB tuplestore/temp file as above, for a count).
	out.Count, _ = scalar[int](ctx, t, `SELECT count(*)::int FROM pg_stat_statements(false)`)
	if m, err := scalar[int](ctx, t, `SELECT current_setting('pg_stat_statements.max')::int`); err == nil {
		out.Max = m
	}
	if caps.VersionNum >= 140000 { // pg_stat_statements_info (dealloc) is PG14+
		out.Dealloc, _ = scalar[int64](ctx, t, `SELECT dealloc FROM pg_stat_statements_info`)
	}
	return out, nil
}

func (queriesCollector) Assemble(c *model.Context, caps conn.Capabilities, s sampled, _ time.Duration, _ Options) {
	if !caps.HasStatStatements {
		// A large fraction of first runs (especially RDS/Aurora) land here, so make
		// it a first-class, provider-specific instruction rather than a dead end.
		c.Queries = &model.Queries{Enabled: false, Section: model.Section{
			Exactness: model.ExactnessUnavailable,
			Reason:    "pg_stat_statements not enabled — " + caps.Provider.PgssInstructions(),
		}}
		return
	}
	sm, ok := s.A.(queriesSample)
	if s.Err != nil || !ok {
		c.Queries = &model.Queries{Enabled: true, Section: unavail(s.Err, "pg_stat_statements read failed")}
		return
	}
	rows := sm.Rows
	q := &model.Queries{Enabled: true, Section: model.Section{Exactness: model.ExactnessCumulative},
		PgssDealloc: sm.Dealloc, PgssCount: sm.Count, PgssMax: sm.Max}
	if len(rows) > 0 {
		q.TotalExecMS = round2(rows[0].TotalExecAll)
	}
	for _, r := range rows {
		item := model.QueryStat{
			// pgss normalizes DML to $N, but stores UTILITY statements (DO blocks,
			// etc.) verbatim — so the text can still carry real literals (PII).
			// Scrub defensively; placeholder-aware scrubbing keeps $N intact.
			QueryID: r.QueryID, Query: conn.ScrubQueryText(r.Query), Calls: r.Calls,
			TotalMS: round2(r.TotalMS), MeanMS: round4(r.MeanMS), MaxMS: round2(r.MaxMS),
			Rows: r.Rows, WALBytes: r.WalBytes,
		}
		if tot := r.SharedBlksHit + r.SharedBlksRead; tot > 0 {
			item.CacheHit = round4p(rate.Ptr(float64(r.SharedBlksHit) / float64(tot)))
		}
		q.Top = append(q.Top, item)
	}
	c.Queries = q
}
