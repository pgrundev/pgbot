package render

import (
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

func resetFixture() DiffInput {
	base := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	return DiffInput{
		Database: "synthetic_test_database", Fingerprint: "fixture123456",
		BaselineAt: base, CurrentAt: base.Add(24 * time.Hour),
		Requested: 24 * time.Hour, Actual: 24 * time.Hour,
	}
}

func TestDiffReport_ResetDoesNotClaimNothingChanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		deltas *model.Deltas
	}{
		{name: "nil", deltas: nil},
		{name: "empty", deltas: &model.Deltas{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := resetFixture()
			in.ResetReason = "server restarted; no comparison available"
			in.Deltas = tc.deltas
			var out strings.Builder
			DiffReport(&out, in)
			if !strings.Contains(out.String(), "statistics were reset") {
				t.Fatalf("missing reset warning: %s", out.String())
			}
			if strings.Contains(out.String(), "nothing material changed") {
				t.Fatalf("unavailable comparison is presented as a clean result:\n%s", out.String())
			}
		})
	}
}

func TestDiffReport_ResetDoesNotPresentCumulativeDelta(t *testing.T) {
	in := resetFixture()
	in.ResetReason = "statistics reset; no comparison available"
	in.Deltas = &model.Deltas{Changes: []model.Delta{{
		ID: "query.mean_ms", Subject: "synthetic-query", Before: 100, After: 1,
		Note: "CUMULATIVE_DELTA_SENTINEL", Severity: "info",
	}}}
	var out strings.Builder
	DiffReport(&out, in)
	if strings.Contains(out.String(), "CUMULATIVE_DELTA_SENTINEL") {
		t.Fatalf("invalid cumulative delta is still rendered after the reset warning:\n%s", out.String())
	}
}

func TestDiffReport_NoResetStillReportsCleanComparison(t *testing.T) {
	in := resetFixture()
	var out strings.Builder
	DiffReport(&out, in)
	if !strings.Contains(out.String(), "nothing material changed") {
		t.Fatalf("normal empty comparison changed: %s", out.String())
	}
}

func TestDiffReport_NoResetStillReportsChanges(t *testing.T) {
	in := resetFixture()
	in.Deltas = &model.Deltas{Changes: []model.Delta{{
		ID: "query.mean_ms", Subject: "synthetic-query", Before: 1, After: 100,
		Note: "VALID_DELTA_SENTINEL", Severity: "warn",
	}}}
	var out strings.Builder
	DiffReport(&out, in)
	if !strings.Contains(out.String(), "VALID_DELTA_SENTINEL") {
		t.Fatalf("valid comparison was suppressed: %s", out.String())
	}
}

func TestDiffReport_EvictionCaveatStillVisible(t *testing.T) {
	in := resetFixture()
	in.PgssEvicted = true
	var out strings.Builder
	DiffReport(&out, in)
	if !strings.Contains(out.String(), "evicted entries") {
		t.Fatalf("eviction warning disappeared: %s", out.String())
	}
}
