package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/store"
)

var errCommandOutput = errors.New("command output rejected")

type rejectingCommandWriter struct{}

func (rejectingCommandWriter) Write([]byte) (int, error) { return 0, errCommandOutput }

func TestDiffCommand_ReturnsOutputError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baselines.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i, at := range []time.Time{now.Add(-2 * time.Hour), now.Add(-time.Minute)} {
		c := &model.Context{
			SchemaVersion: model.SchemaVersion,
			Fingerprint:   "diff-output",
			CollectedAt:   at,
			Server:        model.ServerInfo{Database: "app"},
			Health:        &model.Health{Connections: i + 1},
		}
		if _, err := st.Save(c); err != nil {
			st.Close()
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := newDiffCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetOut(rejectingCommandWriter{})
	cmd.SetArgs([]string{"--store", path, "--since", "1h", "--no-color"})
	err = cmd.Execute()
	if !errors.Is(err, errCommandOutput) {
		t.Fatalf("command error = %v, want output error (the root maps this execution failure to exit 3)", err)
	}
}
