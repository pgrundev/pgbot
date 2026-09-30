package findings

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

var archiverClockEpoch = time.Date(2024, 2, 3, 12, 0, 0, 0, time.UTC)

func archiverClockContext(age time.Duration) *model.Context {
	lastArchived := archiverClockEpoch.Add(-age)
	statsReset := archiverClockEpoch.Add(-24 * time.Hour)
	flow := 4096.0
	return &model.Context{
		SchemaVersion: model.SchemaVersion,
		CollectedAt:   archiverClockEpoch,
		Server:        model.ServerInfo{Database: "app", VersionNum: 160000},
		Window:        model.Window{SampleSeconds: 5},
		WAL: &model.WAL{
			Section:     model.Section{Exactness: model.ExactnessSampled},
			BytesPerSec: &flow,
		},
		Settings: &model.Settings{
			Section: model.Section{Exactness: model.ExactnessScraped},
			Params: map[string]string{
				"archive_mode":    "on",
				"archive_timeout": "5min",
				"wal_level":       "replica",
			},
		},
		Archiver: &model.Archiver{
			Section:           model.Section{Exactness: model.ExactnessScraped},
			ArchivedCount:     42,
			LastArchivedWAL:   "00000001000000000000002A",
			LastArchivedTime:  &lastArchived,
			StatsReset:        &statsReset,
			HasArchiveCommand: true,
		},
		Findings: []model.Finding{},
	}
}

func TestWalArchiving_stallUsesSerializedCollectionTime(t *testing.T) {
	original := archiverClockContext(30 * time.Minute)
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}

	decode := func() *model.Context {
		t.Helper()
		var c model.Context
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		return &c
	}
	first, second := decode(), decode()
	firstFindings, secondFindings := Compute(first), Compute(second)
	if !reflect.DeepEqual(firstFindings, secondFindings) {
		t.Fatalf("the same serialized Context produced different findings:\nfirst:  %+v\nsecond: %+v", firstFindings, secondFindings)
	}
	if f := has(firstFindings, "archiving_stalled"); f != nil {
		t.Fatalf("snapshot was healthy at collection (30m < 1h); viewing it later must not invent a stall: %+v", f)
	}
	after, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, raw) {
		t.Fatalf("Compute mutated its Context input:\nbefore: %s\nafter:  %s", raw, after)
	}
}

func TestWalArchiving_stallThresholdBoundariesUseCollectedAt(t *testing.T) {
	tests := []struct {
		name string
		age  time.Duration
		want bool
	}{
		{name: "below", age: time.Hour - time.Second, want: false},
		{name: "equal", age: time.Hour, want: false},
		{name: "above", age: time.Hour + time.Second, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := has(Compute(archiverClockContext(tt.age)), "archiving_stalled")
			got := f != nil
			if got != tt.want {
				t.Fatalf("age %s: archiving_stalled present=%v, want %v", tt.age, got, tt.want)
			}
			if tt.name == "above" {
				if len(f.Evidence) != 1 || f.Evidence[0] != "last archived 1h ago; archive_timeout=5min" {
					t.Fatalf("displayed age must come from the same Context interval as the decision: %v", f.Evidence)
				}
			}
		})
	}
}

func TestWalArchiving_stallRequiresKnownCollectionTime(t *testing.T) {
	c := archiverClockContext(2 * time.Hour)
	c.CollectedAt = time.Time{}
	if f := has(Compute(c), "archiving_stalled"); f != nil {
		t.Fatalf("zero CollectedAt is unknown; Compute must not invent a wall-clock fallback: %+v", f)
	}
}

func TestWalArchiving_futureArchiveTimeIsNotStalled(t *testing.T) {
	c := archiverClockContext(0)
	future := c.CollectedAt.Add(time.Minute)
	c.Archiver.LastArchivedTime = &future
	if f := has(Compute(c), "archiving_stalled"); f != nil {
		t.Fatalf("future-dated last_archived_time must not become a stall: %+v", f)
	}
}

func TestWalArchiving_stallControls(t *testing.T) {
	t.Run("managed provider is downgraded", func(t *testing.T) {
		c := archiverClockContext(2 * time.Hour)
		c.Server.Provider = "rds"
		f := has(Compute(c), "archiving_stalled")
		if f == nil || f.Severity != model.SeverityInfo || len(f.Caveats) == 0 {
			t.Fatalf("managed-provider stall must remain info with a caveat, got %+v", f)
		}
	})

	t.Run("standby collector omits archiver", func(t *testing.T) {
		c := archiverClockContext(2 * time.Hour)
		c.Server.InRecovery = true
		c.Archiver = nil
		if f := has(Compute(c), "archiving_stalled"); f != nil {
			t.Fatalf("standby Context without an archiver section must not emit a primary-only stall: %+v", f)
		}
	})

	t.Run("no WAL sample", func(t *testing.T) {
		c := archiverClockContext(2 * time.Hour)
		c.WAL = nil
		if f := has(Compute(c), "archiving_stalled"); f != nil {
			t.Fatalf("without sampled WAL flow, a stall is not established: %+v", f)
		}
	})

	t.Run("newer failure selects failing finding", func(t *testing.T) {
		c := archiverClockContext(2 * time.Hour)
		failed := c.CollectedAt.Add(-time.Minute)
		c.Archiver.LastFailedTime = &failed
		if f := has(Compute(c), "archiving_failing"); f == nil || f.Severity != model.SeverityCritical {
			t.Fatalf("newer archive failure must remain critical, got %+v", f)
		}
		if f := has(Compute(c), "archiving_stalled"); f != nil {
			t.Fatalf("the explicit failure path must not also emit a silent stall: %+v", f)
		}
	})
}
