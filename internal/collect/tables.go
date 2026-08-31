package collect

import (
	"context"
	_ "embed"
	"strings"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/crdbhttp"
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
}

type tablesSample struct {
	Rows       []tableRow
	DBSize     int64
	Partitions []partitionRow
}

type cockroachTablesSample struct {
	Configured bool
	Snapshot   crdbhttp.TableMetadataSnapshot
}

func (tablesCollector) Name() string                     { return "tables" }
func (tablesCollector) Kind() Kind                       { return KindGauge }
func (tablesCollector) Available(conn.Capabilities) bool { return true }

func (tablesCollector) Sample(ctx context.Context, t *conn.Target, caps conn.Capabilities) (any, error) {
	return (tablesCollector{}).SampleWithOptions(ctx, t, caps, Options{})
}

func (tablesCollector) SampleWithOptions(ctx context.Context, t *conn.Target, caps conn.Capabilities, opts Options) (any, error) {
	if caps.IsCockroachDB() {
		sm := cockroachTablesSample{}
		if opts.CockroachHTTP == nil || !opts.CockroachHTTP.HasAdmin() {
			return sm, nil
		}
		sm.Configured = true
		snapshot, err := opts.CockroachHTTP.TableMetadata(ctx, caps.Database)
		sm.Snapshot = snapshot
		return sm, err
	}
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

func (tablesCollector) Assemble(c *model.Context, caps conn.Capabilities, s sampled, _ time.Duration, _ Options) {
	if caps.IsCockroachDB() {
		assembleCockroachTables(c, s)
		return
	}
	ts, ok := s.A.(tablesSample)
	if s.Err != nil || !ok {
		c.Tables = &model.Tables{Section: unavail(s.Err, "pg_stat_user_tables unavailable")}
		return
	}
	tbl := &model.Tables{
		Section: model.Section{Exactness: model.ExactnessScraped}, DBSizeBytes: ts.DBSize,
		Total: len(ts.Rows), Scanned: len(ts.Rows), StatsSource: "pg_stat_user_tables", SizeKind: "postgresql_database_size",
	}
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
		})
	}
	c.Tables = tbl
}

func assembleCockroachTables(c *model.Context, s sampled) {
	sm, ok := s.A.(cockroachTablesSample)
	if !ok || !sm.Configured {
		c.Tables = &model.Tables{Section: unavail(nil, "configure --crdb-admin-url for CockroachDB table metadata")}
		return
	}
	if s.Err != nil {
		c.Tables = &model.Tables{Section: unavail(s.Err, "CockroachDB table metadata read failed")}
		return
	}
	total := sm.Snapshot.TotalTables
	if total == 0 {
		total = sm.Snapshot.Database.TableCount
	}
	tbl := &model.Tables{
		Section:     model.Section{Exactness: model.ExactnessScraped},
		DBSizeBytes: sm.Snapshot.Database.SizeBytes, Total: int(total), Scanned: len(sm.Snapshot.Tables),
		StatsSource: "cockroachdb_table_metadata_api", SizeKind: "replicated_disk_estimate",
		MetadataBounded: int64(len(sm.Snapshot.Tables)) < total,
	}
	for _, r := range sm.Snapshot.Tables {
		var metadataAt *time.Time
		if !r.LastUpdated.IsZero() {
			metadataAt = utcTime(&r.LastUpdated)
		}
		st := model.TableStat{
			Database: r.Database, TableID: r.TableID, Schema: r.Schema, Name: r.Table,
			TotalBytes: r.ReplicationSizeBytes, ReplicatedBytes: r.ReplicationSizeBytes,
			LiveDataBytes: r.TotalLiveDataBytes, DataBytes: r.TotalDataBytes, LiveDataRatio: round4(r.PercentLiveData),
			RangeCount: r.RangeCount, ReplicaCount: r.ReplicaCount, StoreIDs: r.StoreIDs,
			ColumnCount: r.ColumnCount, IndexCount: r.IndexCount, AutoStatsEnabled: r.AutoStatsEnabled,
			StatsLastUpdated: utcTime(r.StatsLastUpdated), MetadataLastUpdated: metadataAt,
			MetadataError: conn.RedactConnString(r.LastUpdateError),
		}
		enrichCockroachTableHotRanges(c, &st)
		tbl.Top = append(tbl.Top, st)
		if metadataAt != nil {
			if tbl.MetadataOldestAt == nil || metadataAt.Before(*tbl.MetadataOldestAt) {
				t := *metadataAt
				tbl.MetadataOldestAt = &t
			}
			if tbl.MetadataNewestAt == nil || metadataAt.After(*tbl.MetadataNewestAt) {
				t := *metadataAt
				tbl.MetadataNewestAt = &t
			}
		}
	}
	if len(sm.Snapshot.Tables) == 0 && sm.Snapshot.Database.UpdatedAt != nil {
		tbl.MetadataOldestAt = utcTime(sm.Snapshot.Database.UpdatedAt)
		tbl.MetadataNewestAt = utcTime(sm.Snapshot.Database.UpdatedAt)
	}
	c.Tables = tbl
}

func enrichCockroachTableHotRanges(c *model.Context, table *model.TableStat) {
	if c.Health == nil || c.Health.Cockroach == nil {
		return
	}
	for _, hot := range c.Health.Cockroach.Hot {
		if len(hot.Databases) > 0 && !stringSliceHas(hot.Databases, table.Database) {
			continue
		}
		if hot.Schema != "" && hot.Schema != table.Schema {
			continue
		}
		if !stringSliceHas(hot.Tables, table.Name) {
			continue
		}
		table.TopHotRangeCount++
		table.TopHotRangeQPS = round2(table.TopHotRangeQPS + hot.QPS)
		table.TopHotRangeCPUCores = round4(table.TopHotRangeCPUCores + hot.CPUCores)
	}
}

func stringSliceHas(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}
