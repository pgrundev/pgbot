package diff

import (
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

func change(d *model.Deltas, id string) *model.Delta {
	for i := range d.Changes {
		if d.Changes[i].ID == id {
			return &d.Changes[i]
		}
	}
	return nil
}

func TestCompute_detectsRegressions(t *testing.T) {
	base := &model.Context{
		Server:  model.ServerInfo{Database: "app"},
		Health:  &model.Health{Connections: 20},
		Tables:  &model.Tables{DBSizeBytes: 10 << 30, Top: []model.TableStat{{Schema: "public", Name: "orders", SeqScans: 100, DeadRatio: 0.02}}},
		Queries: &model.Queries{Enabled: true, Top: []model.QueryStat{{QueryID: 1, MeanMS: 2.0, TotalMS: 200}}},
	}
	now := &model.Context{
		CollectedAt: time.Now().UTC(),
		Server:      model.ServerInfo{Database: "app"},
		Health:      &model.Health{Connections: 80}, // +300%
		Tables:      &model.Tables{DBSizeBytes: 12 << 30, Top: []model.TableStat{{Schema: "public", Name: "orders", SeqScans: 5000, DeadRatio: 0.25}}},
		Queries: &model.Queries{Enabled: true, Top: []model.QueryStat{
			{QueryID: 1, MeanMS: 20.0, TotalMS: 2000}, // 2ms -> 20ms
			{QueryID: 2, MeanMS: 5.0, TotalMS: 5000},  // new
		}},
	}
	d := Compute(now, &Baseline{CollectedAt: base.CollectedAt, Context: base}, nil)
	if d == nil {
		t.Fatal("expected deltas")
	}
	if c := change(d, "query.mean_ms"); c == nil || c.Severity != model.SeverityWarn {
		t.Errorf("expected query.mean_ms warn, got %+v", c)
	}
	if c := change(d, "query.new"); c == nil {
		t.Error("expected query.new for queryid 2")
	}
	if c := change(d, "table.seq_scans"); c == nil {
		t.Error("expected table.seq_scans surge")
	}
	if c := change(d, "table.dead_ratio"); c == nil {
		t.Error("expected table.dead_ratio climb")
	}
	if c := change(d, "database.size_bytes"); c == nil {
		t.Error("expected database.size_bytes growth")
	}
	if c := change(d, "health.connections"); c == nil {
		t.Error("expected health.connections step")
	}
}

func TestCompute_stableWorkloadNoNoise(t *testing.T) {
	mk := func() *model.Context {
		return &model.Context{
			Server:  model.ServerInfo{Database: "app"},
			Health:  &model.Health{Connections: 20},
			Tables:  &model.Tables{DBSizeBytes: 10 << 30, Top: []model.TableStat{{Schema: "public", Name: "orders", SeqScans: 100, DeadRatio: 0.02}}},
			Queries: &model.Queries{Enabled: true, Top: []model.QueryStat{{QueryID: 1, MeanMS: 2.0}}},
		}
	}
	d := Compute(mk(), &Baseline{Context: mk()}, nil)
	if len(d.Changes) != 0 {
		t.Errorf("stable workload should produce no changes, got %+v", d.Changes)
	}
}

func TestCompute_noBaselineReturnsNil(t *testing.T) {
	if Compute(&model.Context{}, nil, nil) != nil {
		t.Error("no baseline must yield nil deltas")
	}
}

func TestQueryMean_belowAbsoluteThresholdIgnored(t *testing.T) {
	base := &model.Context{Queries: &model.Queries{Enabled: true, Top: []model.QueryStat{{QueryID: 1, MeanMS: 1.0}}}}
	now := &model.Context{Queries: &model.Queries{Enabled: true, Top: []model.QueryStat{{QueryID: 1, MeanMS: 3.0}}}} // +200% but only +2ms
	d := Compute(now, &Baseline{Context: base}, nil)
	if change(d, "query.mean_ms") != nil {
		t.Error("a change under the 5ms absolute floor must not fire")
	}
}

func TestReplicaGone_detectsCardinalityDropForSharedApplicationName(t *testing.T) {
	base := &model.Context{Replication: &model.Replication{Replicas: []model.ReplicaRow{
		{AppName: "walreceiver", ClientAddr: "10.0.0.11"},
		{AppName: "walreceiver", ClientAddr: "10.0.0.12"},
	}}}
	now := &model.Context{Replication: &model.Replication{Replicas: []model.ReplicaRow{
		{AppName: "walreceiver", ClientAddr: "10.0.0.11"},
	}}}

	d := Compute(now, &Baseline{Context: base}, nil)
	c := change(d, "replication.standby_gone")
	if c == nil {
		t.Fatal("a 2-to-1 standby count drop must be reported even when application_name is shared")
	}
	if c.Subject != "walreceiver" || c.Before != 2 || c.After != 1 {
		t.Fatalf("standby cardinality delta = %+v, want walreceiver 2 -> 1", c)
	}
}
