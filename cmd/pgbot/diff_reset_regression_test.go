package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/store"
)

// Exercise the actual SQLite store, offline resolver, and the JSON payload shared
// by the CLI and MCP. This test does not need a live PostgreSQL server.
func TestResolveDiff_ResetSafeJSON(t *testing.T) {
	for _, mode := range []string{"first reset", "later reset", "restart", "no reset"} {
		t.Run(mode, func(t *testing.T) {
			path, fp := writeResetDiffSnapshots(t, mode)
			r, err := resolveDiff(path, fp, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(diffJSON(r))
			if err != nil {
				t.Fatal(err)
			}
			var out struct {
				Reset   string          `json:"stats_reset_between"`
				Changes json.RawMessage `json:"changes"`
			}
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatal(err)
			}
			if mode == "no reset" {
				if out.Reset != "" || r.Deltas == nil || len(r.Deltas.Changes) != 1 {
					t.Fatalf("normal stored comparison was lost: %s", raw)
				}
				return
			}
			if out.Reset == "" {
				t.Fatalf("reset caveat missing: %s", raw)
			}
			if r.Deltas != nil || string(out.Changes) != "[]" {
				t.Fatalf("reset-crossing JSON must contain an empty changes array: %s", raw)
			}
		})
	}
}

func writeResetDiffSnapshots(t *testing.T, mode string) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "baselines.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	}()
	at := time.Now().UTC().Truncate(time.Second).Add(-3 * time.Hour)
	fp := "reset-diff-test"
	base := &model.Context{
		SchemaVersion: model.SchemaVersion, Fingerprint: fp, CollectedAt: at,
		Server:  model.ServerInfo{Database: "synthetic_test_database"},
		Queries: &model.Queries{Enabled: true, Top: []model.QueryStat{{QueryID: 42, MeanMS: 100, TotalMS: 1000}}},
	}
	cur := &model.Context{
		SchemaVersion: model.SchemaVersion, Fingerprint: fp, CollectedAt: at.Add(2 * time.Hour),
		Server:  model.ServerInfo{Database: "synthetic_test_database"},
		Queries: &model.Queries{Enabled: true, Top: []model.QueryStat{{QueryID: 42, MeanMS: 1, TotalMS: 10}}},
	}
	old, changed := at.Add(-time.Hour), at.Add(time.Hour)
	switch mode {
	case "first reset":
		cur.Window.StatsResetAt = &changed
	case "later reset":
		base.Window.StatsResetAt, cur.Window.StatsResetAt = &old, &changed
	case "restart":
		base.Window.PostmasterStartAt, cur.Window.PostmasterStartAt = &old, &changed
	}
	for _, c := range []*model.Context{base, cur} {
		if _, err := st.Save(c); err != nil {
			t.Fatal(err)
		}
	}
	return path, fp
}
