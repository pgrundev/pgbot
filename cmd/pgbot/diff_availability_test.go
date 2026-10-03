package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/store"
)

func TestResolveDiffPreservesMissingObservationsThroughSQLite(t *testing.T) {
	tests := []struct {
		name       string
		baseline   func() *model.Context
		current    func() *model.Context
		checkExact func(*testing.T, *diffResult)
	}{
		{
			name: "unavailable baseline then collection recovers",
			baseline: func() *model.Context {
				return &model.Context{
					Health:      &model.Health{Section: model.Section{Exactness: model.ExactnessUnavailable}},
					Queries:     &model.Queries{Section: model.Section{Exactness: model.ExactnessUnavailable}, Enabled: true},
					Replication: &model.Replication{Section: model.Section{Exactness: model.ExactnessUnavailable}},
					Archiver:    &model.Archiver{Section: model.Section{Exactness: model.ExactnessUnavailable}},
				}
			},
			current: func() *model.Context {
				return &model.Context{
					Health: &model.Health{Section: model.Section{Exactness: model.ExactnessSampled}, Connections: 100},
					Queries: &model.Queries{Section: model.Section{Exactness: model.ExactnessCumulative}, Enabled: true, Top: []model.QueryStat{
						{QueryID: 1, MeanMS: 10, TotalMS: 100},
					}},
					Replication: &model.Replication{Section: model.Section{Exactness: model.ExactnessScraped}, Replicas: []model.ReplicaRow{{AppName: "standby-a"}}},
					Archiver:    &model.Archiver{Section: model.Section{Exactness: model.ExactnessScraped}, FailedCount: 3},
				}
			},
			checkExact: func(t *testing.T, r *diffResult) {
				if r.Baseline.Context.Queries.Exactness != model.ExactnessUnavailable ||
					r.Baseline.Context.Archiver.Exactness != model.ExactnessUnavailable {
					t.Fatal("SQLite JSON round trip lost baseline unavailability")
				}
			},
		},
		{
			name: "current collection fails after a measured baseline",
			baseline: func() *model.Context {
				return &model.Context{
					Health:      &model.Health{Section: model.Section{Exactness: model.ExactnessSampled}, Connections: 100},
					Replication: &model.Replication{Section: model.Section{Exactness: model.ExactnessScraped}, Replicas: []model.ReplicaRow{{AppName: "standby-a"}}},
				}
			},
			current: func() *model.Context {
				return &model.Context{
					Health:      &model.Health{Section: model.Section{Exactness: model.ExactnessUnavailable}},
					Replication: &model.Replication{Section: model.Section{Exactness: model.ExactnessUnavailable}},
				}
			},
			checkExact: func(t *testing.T, r *diffResult) {
				if r.Current.Context.Health.Exactness != model.ExactnessUnavailable ||
					r.Current.Context.Replication.Exactness != model.ExactnessUnavailable {
					t.Fatal("SQLite JSON round trip lost current unavailability")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "baselines.db")
			st, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			baseAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
			base := tt.baseline()
			base.SchemaVersion = model.SchemaVersion
			base.Fingerprint = "availability-test"
			base.CollectedAt = baseAt
			base.Server.Database = "app"
			current := tt.current()
			current.SchemaVersion = model.SchemaVersion
			current.Fingerprint = base.Fingerprint
			current.CollectedAt = baseAt.Add(time.Hour)
			current.Server.Database = "app"
			if _, err := st.Save(base); err != nil {
				t.Fatal(err)
			}
			if _, err := st.Save(current); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}

			r, err := resolveDiff(path, base.Fingerprint, 30*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if got := deltasOrEmpty(r.Deltas); len(got) != 0 {
				t.Fatalf("persisted missing observations must not become changes, got %+v", got)
			}
			tt.checkExact(t, r)
		})
	}
}
