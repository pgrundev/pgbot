package diff

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

func section(exactness string) model.Section {
	return model.Section{Exactness: exactness}
}

func ids(d *model.Deltas) []string {
	out := make([]string, 0, len(d.Changes))
	for _, delta := range d.Changes {
		out = append(out, delta.ID)
	}
	return out
}

func TestComputeMissingSectionsAreNotMeasurements(t *testing.T) {
	positive := func() *model.Context {
		return &model.Context{
			CollectedAt: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC),
			Server:      model.ServerInfo{Database: "app"},
			Health:      &model.Health{Section: section(model.ExactnessSampled), Connections: 100},
			Queries: &model.Queries{Section: section(model.ExactnessCumulative), Enabled: true, Top: []model.QueryStat{
				{QueryID: 1, MeanMS: 10, TotalMS: 100},
			}},
			Replication: &model.Replication{Section: section(model.ExactnessScraped), Replicas: []model.ReplicaRow{{AppName: "standby-a"}}},
			Archiver:    &model.Archiver{Section: section(model.ExactnessScraped), FailedCount: 5},
		}
	}
	unavailable := func() *model.Context {
		return &model.Context{
			CollectedAt: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC),
			Server:      model.ServerInfo{Database: "app"},
			Health:      &model.Health{Section: section(model.ExactnessUnavailable)},
			Queries:     &model.Queries{Section: section(model.ExactnessUnavailable), Enabled: true},
			Replication: &model.Replication{Section: section(model.ExactnessUnavailable)},
			Archiver:    &model.Archiver{Section: section(model.ExactnessUnavailable)},
		}
	}

	tests := []struct {
		name string
		base *model.Context
		now  *model.Context
	}{
		{name: "current unavailable", base: positive(), now: unavailable()},
		{name: "baseline unavailable then collection recovers", base: unavailable(), now: positive()},
		{name: "current sections omitted", base: positive(), now: &model.Context{}},
		{name: "baseline sections omitted", base: &model.Context{}, now: positive()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Compute(tt.now, &Baseline{CollectedAt: tt.base.CollectedAt, Context: tt.base}, nil)
			if len(d.Changes) != 0 {
				t.Fatalf("missing observations must not produce changes, got %v", ids(d))
			}
		})
	}
}

func TestComputeSuccessfulEmptySectionsRemainMeasurements(t *testing.T) {
	baseAt := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	base := &model.Context{
		CollectedAt: baseAt,
		Server:      model.ServerInfo{Database: "app"},
		Health:      &model.Health{Section: section(model.ExactnessSampled), Connections: 100},
		Queries:     &model.Queries{Section: section(model.ExactnessCumulative), Enabled: true},
		Replication: &model.Replication{Section: section(model.ExactnessScraped), Replicas: []model.ReplicaRow{{AppName: "standby-a"}}},
		Archiver:    &model.Archiver{Section: section(model.ExactnessScraped)},
	}
	now := &model.Context{
		CollectedAt: baseAt.Add(time.Hour),
		Server:      model.ServerInfo{Database: "app"},
		Health:      &model.Health{Section: section(model.ExactnessSampled), Connections: 0},
		Queries: &model.Queries{Section: section(model.ExactnessCumulative), Enabled: true, Top: []model.QueryStat{
			{QueryID: 2, MeanMS: 10, TotalMS: 200},
		}},
		Replication: &model.Replication{Section: section(model.ExactnessScraped)},
		Archiver:    &model.Archiver{Section: section(model.ExactnessScraped), FailedCount: 2},
	}

	d := Compute(now, &Baseline{CollectedAt: baseAt, Context: base}, nil)
	for _, id := range []string{"health.connections", "query.new", "replication.standby_gone", "archiver.failed_count"} {
		if change(d, id) == nil {
			t.Errorf("successful empty/positive comparison should produce %s; got %v", id, ids(d))
		}
	}
}

func TestComputeGenuineLossAndAdditionRemainVisible(t *testing.T) {
	baseAt := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	base := &model.Context{
		Queries: &model.Queries{Section: section(model.ExactnessCumulative), Enabled: true, Top: []model.QueryStat{
			{QueryID: 1, MeanMS: 10, TotalMS: 100},
		}},
		Replication: &model.Replication{Section: section(model.ExactnessScraped), Replicas: []model.ReplicaRow{
			{AppName: "standby-a"}, {AppName: "standby-b"},
		}},
	}
	now := &model.Context{
		CollectedAt: baseAt.Add(time.Hour),
		Queries: &model.Queries{Section: section(model.ExactnessCumulative), Enabled: true, Top: []model.QueryStat{
			{QueryID: 1, MeanMS: 10, TotalMS: 150}, {QueryID: 2, MeanMS: 8, TotalMS: 80},
		}},
		Replication: &model.Replication{Section: section(model.ExactnessScraped), Replicas: []model.ReplicaRow{
			{AppName: "standby-b"}, {AppName: "standby-c"},
		}},
	}

	d := Compute(now, &Baseline{CollectedAt: baseAt, Context: base}, nil)
	if got := change(d, "replication.standby_gone"); got == nil || got.Subject != "standby-a" {
		t.Fatalf("expected the genuine standby loss, got %+v", got)
	}
	if got := change(d, "query.new"); got == nil || got.Subject != shortID(2) {
		t.Fatalf("expected the genuine top-query addition, got %+v", got)
	}
	if len(d.Changes) != 2 {
		t.Fatalf("standby/query additions must not hide or invent changes, got %v", ids(d))
	}
}

func TestComputeUnavailableHealthCanRetainMeasuredConnections(t *testing.T) {
	baseAt := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	base := &model.Context{
		Server: model.ServerInfo{Database: "app"},
		Health: &model.Health{Section: section(model.ExactnessSampled), Connections: 100},
	}
	now := &model.Context{
		Server: model.ServerInfo{Database: "app"},
		Health: &model.Health{Section: section(model.ExactnessUnavailable), Connections: 20},
	}

	d := Compute(now, &Baseline{CollectedAt: baseAt, Context: base}, nil)
	if got := change(d, "health.connections"); got == nil || got.Before != 100 || got.After != 20 {
		t.Fatalf("a retained positive connection gauge remains comparable despite rate unavailability, got %+v", got)
	}
}

func TestComputeLegacySectionsNeedEvidenceBeforeAbsenceIsTrusted(t *testing.T) {
	baseAt := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	t.Run("empty legacy baseline does not make current data new", func(t *testing.T) {
		base := &model.Context{
			Health: &model.Health{}, Queries: &model.Queries{Enabled: true},
			Replication: &model.Replication{}, Archiver: &model.Archiver{},
		}
		now := &model.Context{
			Server: model.ServerInfo{Database: "app"},
			Health: &model.Health{Section: section(model.ExactnessSampled), Connections: 100},
			Queries: &model.Queries{Section: section(model.ExactnessCumulative), Enabled: true, Top: []model.QueryStat{
				{QueryID: 1, MeanMS: 10, TotalMS: 100},
			}},
			Replication: &model.Replication{Section: section(model.ExactnessScraped), Replicas: []model.ReplicaRow{{AppName: "standby-a"}}},
			Archiver:    &model.Archiver{Section: section(model.ExactnessScraped), FailedCount: 2},
		}
		d := Compute(now, &Baseline{CollectedAt: baseAt, Context: base}, nil)
		if len(d.Changes) != 0 {
			t.Fatalf("an empty legacy section is unknown, not known-empty; got %v", ids(d))
		}
	})

	t.Run("empty legacy current does not imply loss", func(t *testing.T) {
		base := &model.Context{
			Server:      model.ServerInfo{Database: "app"},
			Health:      &model.Health{Section: section(model.ExactnessSampled), Connections: 100},
			Replication: &model.Replication{Section: section(model.ExactnessScraped), Replicas: []model.ReplicaRow{{AppName: "standby-a"}}},
		}
		now := &model.Context{Server: model.ServerInfo{Database: "app"}, Health: &model.Health{}, Replication: &model.Replication{}}
		d := Compute(now, &Baseline{CollectedAt: baseAt, Context: base}, nil)
		if len(d.Changes) != 0 {
			t.Fatalf("an empty legacy current section is unknown, not known-empty; got %v", ids(d))
		}
	})

	t.Run("positive legacy values remain usable", func(t *testing.T) {
		base := &model.Context{
			Server:      model.ServerInfo{Database: "app"},
			Health:      &model.Health{Connections: 100},
			Queries:     &model.Queries{Enabled: true, Top: []model.QueryStat{{QueryID: 1, MeanMS: 10, TotalMS: 100}}},
			Replication: &model.Replication{Replicas: []model.ReplicaRow{{AppName: "standby-a"}}},
			Archiver:    &model.Archiver{FailedCount: 5},
		}
		now := &model.Context{
			CollectedAt: baseAt.Add(time.Hour), Server: model.ServerInfo{Database: "app"},
			Health: &model.Health{Section: section(model.ExactnessSampled), Connections: 20},
			Queries: &model.Queries{Section: section(model.ExactnessCumulative), Enabled: true, Top: []model.QueryStat{
				{QueryID: 1, MeanMS: 10, TotalMS: 150}, {QueryID: 2, MeanMS: 8, TotalMS: 80},
			}},
			Replication: &model.Replication{Section: section(model.ExactnessScraped)},
			Archiver:    &model.Archiver{Section: section(model.ExactnessScraped), FailedCount: 6},
		}
		d := Compute(now, &Baseline{CollectedAt: baseAt, Context: base}, nil)
		for _, id := range []string{"health.connections", "query.new", "replication.standby_gone", "archiver.failed_count"} {
			if change(d, id) == nil {
				t.Errorf("positive legacy evidence should preserve %s comparisons; got %v", id, ids(d))
			}
		}
	})

	t.Run("legacy health rates prove a zero gauge was collected", func(t *testing.T) {
		zero := 0.0
		base := &model.Context{
			Server: model.ServerInfo{Database: "app"},
			Health: &model.Health{Connections: 100},
		}
		now := &model.Context{
			Server: model.ServerInfo{Database: "app"},
			Health: &model.Health{Connections: 0, TPS: &zero},
		}
		d := Compute(now, &Baseline{CollectedAt: baseAt, Context: base}, nil)
		if change(d, "health.connections") == nil {
			t.Fatalf("a legacy rate is positive evidence that the zero connection gauge was collected; got %v", ids(d))
		}
	})
}

func TestComputeAvailabilityIsDeterministicAndDoesNotMutateContexts(t *testing.T) {
	baseAt := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	base := &model.Context{
		Server:      model.ServerInfo{Database: "app"},
		Health:      &model.Health{Section: section(model.ExactnessSampled), Connections: 100},
		Replication: &model.Replication{Section: section(model.ExactnessScraped), Replicas: []model.ReplicaRow{{AppName: "standby-a"}}},
	}
	now := &model.Context{
		CollectedAt: baseAt.Add(time.Hour), Server: model.ServerInfo{Database: "app"},
		Health:      &model.Health{Section: section(model.ExactnessUnavailable)},
		Replication: &model.Replication{Section: section(model.ExactnessUnavailable)},
	}
	baseBefore, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	nowBefore, err := json.Marshal(now)
	if err != nil {
		t.Fatal(err)
	}

	first := Compute(now, &Baseline{CollectedAt: baseAt, Context: base}, nil)
	second := Compute(now, &Baseline{CollectedAt: baseAt, Context: base}, nil)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same inputs produced different deltas:\nfirst:  %+v\nsecond: %+v", first, second)
	}
	baseAfter, _ := json.Marshal(base)
	nowAfter, _ := json.Marshal(now)
	if !bytes.Equal(baseBefore, baseAfter) || !bytes.Equal(nowBefore, nowAfter) {
		t.Fatal("Compute mutated one of its Context inputs")
	}
}
