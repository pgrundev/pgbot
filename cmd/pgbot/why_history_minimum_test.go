package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/store"
)

func TestWhy_ThreeSnapshotsReportsInsufficientHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baselines.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := 0; i < 4; i++ {
		at := now.Add(time.Duration(i-4) * 24 * time.Hour)
		if i == 0 {
			at = now.Add(-30 * 24 * time.Hour)
		}
		c := &model.Context{
			SchemaVersion: model.SchemaVersion,
			Fingerprint:   "history-minimum",
			CollectedAt:   at,
			Server:        model.ServerInfo{Database: "app"},
		}
		if _, err := st.Save(c); err != nil {
			st.Close()
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	out := runWhyCmd(t, "--store", path, "--no-color")
	for _, want := range []string{"at least 4", "pgbot inspect", "--window"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "nothing got measurably worse") {
		t.Fatalf("insufficient history was rendered as a clean result:\n%s", out)
	}
}
