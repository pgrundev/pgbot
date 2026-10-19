package collect

import (
	"context"
	_ "embed"
	"strings"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
)

// relFalse reports whether a reloption boolean string means "off" (autovacuum_enabled=false).
func relFalse(s *string) bool {
	if s == nil {
		return false
	}
	switch strings.ToLower(*s) {
	case "false", "off", "0", "no":
		return true
	}
	return false
}

//go:embed sql/tables.sql
var sqlTables string

//go:embed sql/partitions.sql
var sqlPartitions string

// tables = pg_stat_user_tables plus the total database size.
type tablesCollector struct{}

type tableRow struct {
	Schema              string     `db:"schema"`
	Table               string     `db:"table"`
	TotalBytes          int64      `db:"total_bytes"`
	LiveTuples          int64      `db:"live_tuples"`
	DeadTuples          int64      `db:"dead_tuples"`
	SeqScans            int64      `db:"seq_scans"`
	IndexScans          int64      `db:"index_scans"`
	ModsSinceAnalyze    int64      `db:"mods_since_analyze"`
	Updates             int64      `db:"updates"`
	HotUpdates          int64      `db:"hot_updates"`
	LastAnalyze         *time.Time `db:"last_analyze"`
	LastAutoanalyze     *time.Time `db:"last_autoanalyze"`
	LastVacuum          *time.Time `db:"last_vacuum"`
	LastAutovacuum      *time.Time `db:"last_autovacuum"`
	RelAnalyzeScale     *float64   `db:"rel_analyze_scale"`
	RelAnalyzeThreshold *float64   `db:"rel_analyze_threshold"`
	AutovacuumCount     int64      `db:"autovacuum_count"`
	RelAutovacEnabled   *string    `db:"rel_autovacuum_enabled"`
	RelVacuumScale      *float64   `db:"rel_vacuum_scale"`
	RelVacuumThreshold  *float64   `db:"rel_vacuum_threshold"`
}

type partitionRow struct {
	Schema     string `db:"schema"`
	Table      string `db:"table"`
	Partitions int    `db:"partitions"`
	TotalBytes int64  `db:"total_bytes"`
	LiveTuples int64  `db:"live_tuples"`
	SeqScans   int64  `db:"seq_scans"`
	IndexScans int64  `db:"index_scans"`
	HotPart    string `db:"hot_partition"`
	HotScans   int64  `db:"hot_scans"`
	BigPart    string `db:"big_partition"`
	BigRows    int64  `db:"big_rows"`
}

type tablesSample struct {
	Rows       []tableRow
	DBSize     int64
	Partitions []partitionRow
}

func (tablesCollector) Name() string                     { return "tables" }
func (tablesCollector) Kind() Kind                       { return KindGauge }
func (tablesCollector) Available(conn.Capabilities) bool { return true }

func (tablesCollector) Sample(ctx context.Context, t *conn.Target, _ conn.Capabilities) (any, error) {
	rows, err := queryMany[tableRow](ctx, t, sqlTables)
	if err != nil {
		return nil, err
	}
	size, err := scalar[int64](ctx, t, `SELECT pg_database_size(current_database())`)
	if err != nil {
		return nil, err
	}
	out := tablesSample{Rows: rows, DBSize: size}
	// Partition rollup is a pure catalog read; a failure must not sink the section.
	out.Partitions, _ = queryMany[partitionRow](ctx, t, sqlPartitions)
	return out, nil
}

func (tablesCollector) Assemble(c *model.Context, _ conn.Capabilities, s sampled, _ time.Duration, _ Options) {
	ts, ok := s.A.(tablesSample)
	if s.Err != nil || !ok {
		c.Tables = &model.Tables{Section: unavail(s.Err, "pg_stat_user_tables unavailable")}
		return
	}
	tbl := &model.Tables{Section: model.Section{Exactness: model.ExactnessScraped}, DBSizeBytes: ts.DBSize}
	for _, r := range ts.Rows {
		dead := 0.0
		if tot := r.LiveTuples + r.DeadTuples; tot > 0 {
			dead = float64(r.DeadTuples) / float64(tot)
		}
		tbl.Top = append(tbl.Top, model.TableStat{
			Schema: r.Schema, Name: r.Table, TotalBytes: r.TotalBytes,
			LiveTuples: r.LiveTuples, DeadTuples: r.DeadTuples, DeadRatio: round4(dead),
			SeqScans: r.SeqScans, IndexScans: r.IndexScans, ModsSinceAnalyze: r.ModsSinceAnalyze,
			Updates: r.Updates, HotUpdates: r.HotUpdates,
			LastAnalyze: r.LastAnalyze, LastAutoanalyze: r.LastAutoanalyze,
			LastVacuum: r.LastVacuum, LastAutovac: r.LastAutovacuum,
			AnalyzeScaleOverride: r.RelAnalyzeScale, AnalyzeThresholdOverride: r.RelAnalyzeThreshold,
			AutovacuumCount: r.AutovacuumCount, AutovacuumDisabled: relFalse(r.RelAutovacEnabled),
			VacuumScaleOverride: r.RelVacuumScale, VacuumThresholdOverride: r.RelVacuumThreshold,
		})
	}
	for _, p := range ts.Partitions {
		tbl.Partitioned = append(tbl.Partitioned, model.PartitionRollup{
			Schema: p.Schema, Name: p.Table, Partitions: p.Partitions, TotalBytes: p.TotalBytes,
			LiveTuples: p.LiveTuples, SeqScans: p.SeqScans, IndexScans: p.IndexScans,
			HotPartition: p.HotPart, HotScans: p.HotScans, BigPartition: p.BigPart, BigRows: p.BigRows,
		})
	}
	c.Tables = tbl
}
