package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

// Saved wait profiles must read back as class shares over a window — the
// baseline `why --duration` compares a live study against.
func TestReadWaitSharesRoundtrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	wp := &model.WaitProfile{Available: true, Samples: 100, Buckets: []model.WaitBucket{
		{Type: "Lock", Count: 25, Events: []model.WaitEvent{{Event: "transactionid", Count: 25}}},
		{Type: "CPU", Count: 75},
	}}
	if err := st.SaveWaitProfile("fp1", at, wp); err != nil {
		t.Fatal(err)
	}

	shares, total, err := ReadWaitShares(st, "fp1", at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if total != 100 {
		t.Errorf("total = %d, want 100", total)
	}
	if shares["Lock"] != 0.25 || shares["CPU"] != 0.75 {
		t.Errorf("shares wrong: %v", shares)
	}

	// Outside the window: empty, not an error.
	shares, total, err = ReadWaitShares(st, "fp1", at.Add(24*time.Hour), at.Add(48*time.Hour))
	if err != nil || total != 0 || len(shares) != 0 {
		t.Errorf("out-of-window must be empty: %v %d %v", shares, total, err)
	}
}
