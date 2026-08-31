package collect

import (
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/crdbhttp"
	"github.com/pgrundev/pgbot/internal/model"
)

func TestAssembleCockroachTablesMetadataAndHotRangeCorrelation(t *testing.T) {
	oldest := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	newest := oldest.Add(time.Hour)
	statsAt := newest.Add(-2 * time.Hour)
	c := &model.Context{Health: &model.Health{Cockroach: &model.CockroachHealth{Hot: []model.CockroachHotRange{{
		Databases: []string{"app"}, Schema: "public", Tables: []string{"orders"}, QPS: 125.5, CPUCores: .25,
	}}}}}
	snapshot := crdbhttp.TableMetadataSnapshot{
		Database:    crdbhttp.DatabaseMetadata{Name: "app", SizeBytes: 8 << 30, TableCount: 2},
		TotalTables: 2,
		Tables: []crdbhttp.TableMetadata{
			{Database: "app", TableID: 51, Schema: "public", Table: "orders", ReplicationSizeBytes: 6 << 30, RangeCount: 30, ReplicaCount: 90, StoreIDs: []int64{1, 2, 3}, TotalLiveDataBytes: 3 << 30, TotalDataBytes: 4 << 30, PercentLiveData: .75, AutoStatsEnabled: true, StatsLastUpdated: &statsAt, LastUpdated: oldest},
			{Database: "app", TableID: 52, Schema: "public", Table: "customers", ReplicationSizeBytes: 2 << 30, LastUpdated: newest},
		},
	}
	assembleCockroachTables(c, sampled{A: cockroachTablesSample{Configured: true, Snapshot: snapshot}})

	if c.Tables == nil || c.Tables.Exactness != model.ExactnessScraped || c.Tables.Total != 2 || c.Tables.DBSizeBytes != 8<<30 {
		t.Fatalf("unexpected table summary: %+v", c.Tables)
	}
	if c.Tables.MetadataOldestAt == nil || !c.Tables.MetadataOldestAt.Equal(oldest) || c.Tables.MetadataNewestAt == nil || !c.Tables.MetadataNewestAt.Equal(newest) {
		t.Fatalf("metadata freshness range missing: %+v", c.Tables)
	}
	orders := c.Tables.Top[0]
	if orders.LiveDataRatio != .75 || orders.RangeCount != 30 || orders.ReplicaCount != 90 || orders.TopHotRangeCount != 1 || orders.TopHotRangeQPS != 125.5 {
		t.Fatalf("table health/correlation missing: %+v", orders)
	}
}

func TestAssembleCockroachTablesRequiresAdminAPI(t *testing.T) {
	c := &model.Context{}
	assembleCockroachTables(c, sampled{A: cockroachTablesSample{}})
	if c.Tables == nil || c.Tables.Exactness != model.ExactnessUnavailable || c.Tables.Reason != "configure --crdb-admin-url for CockroachDB table metadata" {
		t.Fatalf("missing Admin API should be explicit: %+v", c.Tables)
	}
}
