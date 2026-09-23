package store

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

func TestSaveTrendPreservesScalarAvailabilityAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "availability.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	fp := "availability"
	base := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	zero, positiveTPS, positiveCache := 0.0, 12.5, 0.98
	legacyTPS, legacyCache := 3.0, 0.5
	contexts := []*model.Context{
		{
			SchemaVersion: model.SchemaVersion,
			Fingerprint:   fp,
			CollectedAt:   base,
			Server:        model.ServerInfo{Database: "missing"},
		},
		{
			SchemaVersion: model.SchemaVersion,
			Fingerprint:   fp,
			CollectedAt:   base.Add(time.Minute),
			Server:        model.ServerInfo{Database: "unavailable"},
			Health: &model.Health{Section: model.Section{
				Exactness: model.ExactnessUnavailable,
				Reason:    "pg_stat_database unavailable",
			}},
			Tables: &model.Tables{Section: model.Section{
				Exactness: model.ExactnessUnavailable,
				Reason:    "pg_stat_user_tables unavailable",
			}},
			Activity: &model.Activity{Section: model.Section{
				Exactness: model.ExactnessUnavailable,
				Reason:    "pg_stat_activity unavailable",
			}},
		},
		{
			SchemaVersion: model.SchemaVersion,
			Fingerprint:   fp,
			CollectedAt:   base.Add(2 * time.Minute),
			Server:        model.ServerInfo{Database: "observed-zero"},
			Health: &model.Health{
				Section:       model.Section{Exactness: model.ExactnessSampled},
				Connections:   0,
				TPS:           &zero,
				CacheHitRatio: &zero,
			},
			Tables: &model.Tables{
				Section:     model.Section{Exactness: model.ExactnessScraped},
				DBSizeBytes: 0,
			},
			Activity: &model.Activity{
				Section:        model.Section{Exactness: model.ExactnessScraped},
				LongestXactSec: 0,
			},
		},
		{
			SchemaVersion: model.SchemaVersion,
			Fingerprint:   fp,
			CollectedAt:   base.Add(3 * time.Minute),
			Server:        model.ServerInfo{Database: "observed-without-rates"},
			Health: &model.Health{
				Section:     model.Section{Exactness: model.ExactnessSampled},
				Connections: 0,
			},
		},
		{
			SchemaVersion: model.SchemaVersion,
			Fingerprint:   fp,
			CollectedAt:   base.Add(4 * time.Minute),
			Server:        model.ServerInfo{Database: "positive"},
			Health: &model.Health{
				Section:       model.Section{Exactness: model.ExactnessSampled},
				Connections:   9,
				TPS:           &positiveTPS,
				CacheHitRatio: &positiveCache,
			},
			Tables: &model.Tables{
				Section:     model.Section{Exactness: model.ExactnessScraped},
				DBSizeBytes: 1000,
				Top: []model.TableStat{
					{DeadRatio: 0.10},
					{DeadRatio: 0.25},
				},
			},
			Activity: &model.Activity{
				Section:        model.Section{Exactness: model.ExactnessScraped},
				LongestXactSec: 42,
			},
		},
		{
			SchemaVersion: model.SchemaVersion,
			Fingerprint:   fp,
			CollectedAt:   base.Add(5 * time.Minute),
			Server:        model.ServerInfo{Database: "reset-gauges"},
			Health: &model.Health{
				Section:     model.Section{Exactness: model.ExactnessReset, Reason: "counter reset"},
				Connections: 2,
			},
		},
		{
			SchemaVersion: model.SchemaVersion,
			Fingerprint:   fp,
			CollectedAt:   base.Add(6 * time.Minute),
			Server:        model.ServerInfo{Database: "legacy-no-exactness"},
			Health: &model.Health{
				Connections:   4,
				TPS:           &legacyTPS,
				CacheHitRatio: &legacyCache,
			},
			Tables: &model.Tables{
				DBSizeBytes: 500,
			},
			Activity: &model.Activity{
				LongestXactSec: 7,
			},
		},
	}

	// A reset can invalidate sampled rates while leaving the current connection
	// gauge usable (the health collector and PR #73 preserve that measurement).
	contexts = append(contexts, &model.Context{
		SchemaVersion: model.SchemaVersion, Fingerprint: fp,
		CollectedAt: base.Add(7 * time.Minute),
		Health:      &model.Health{Section: model.Section{Exactness: model.ExactnessUnavailable, Reason: "counter epoch changed"}, Connections: 17},
	})
	// A legacy zero with no availability metadata is not newly promoted into a
	// measured-zero observation. Existing positive legacy gauges remain valid.
	contexts = append(contexts, &model.Context{
		SchemaVersion: model.SchemaVersion, Fingerprint: fp, CollectedAt: base.Add(8 * time.Minute), Health: &model.Health{},
	})

	wantJSON := make([]string, len(contexts))
	for i, c := range contexts {
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		wantJSON[i] = string(raw)
		if _, err := st.Save(c); err != nil {
			t.Fatalf("Save context %q: %v", c.Server.Database, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	wants := map[string][]float64{
		"tps":            {0, 12.5, 3},
		"cache_hit":      {0, 0.98, 0.5},
		"connections":    {0, 0, 9, 2, 4, 17},
		"db_size_bytes":  {0, 1000, 500},
		"dead_ratio_max": {0, 0.25, 0},
		"longest_xact_s": {0, 42, 7},
	}
	for column, want := range wants {
		got, err := st.Trend(fp, column, 100)
		if err != nil {
			t.Fatalf("Trend(%q): %v", column, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Trend(%q) = %v, want %v", column, got, want)
		}
	}

	rows, err := st.db.Query(`SELECT context_json FROM snapshots WHERE fingerprint = ? ORDER BY collected_at`, fp)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var gotJSON []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		gotJSON = append(gotJSON, raw)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotJSON, wantJSON) {
		t.Error("Save changed serialized contexts while extracting trend scalars")
	}

	loaded, err := st.LoadRange(fp, base.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != len(contexts) {
		t.Fatalf("LoadRange returned %d contexts, want %d", len(loaded), len(contexts))
	}
	if got := loaded[1].Context.Tables.Section; got.Exactness != model.ExactnessUnavailable || got.Reason != "pg_stat_user_tables unavailable" {
		t.Errorf("unavailable section provenance changed after reopen: %+v", got)
	}
	if loaded[3].Context.Health.TPS != nil || loaded[3].Context.Health.CacheHitRatio != nil {
		t.Errorf("unmeasured pointer rates became observations after reopen: %+v", loaded[3].Context.Health)
	}
}
