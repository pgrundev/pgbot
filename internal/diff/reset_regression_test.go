package diff

import (
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

func resetComparisonFixture() (*model.Context, *Baseline) {
	at := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	base := &model.Context{
		CollectedAt: at,
		Queries:     &model.Queries{Enabled: true, Top: []model.QueryStat{{QueryID: 42, MeanMS: 100, TotalMS: 1000}}},
	}
	cur := &model.Context{
		CollectedAt: at.Add(24 * time.Hour),
		Queries:     &model.Queries{Enabled: true, Top: []model.QueryStat{{QueryID: 42, MeanMS: 1, TotalMS: 10}}},
	}
	return cur, &Baseline{CollectedAt: at, Context: base}
}

func TestCompute_SuppressesResetCrossingComparisons(t *testing.T) {
	for _, name := range []string{"first stats reset", "later stats reset", "postmaster restart"} {
		t.Run(name, func(t *testing.T) {
			cur, base := resetComparisonFixture()
			old := base.CollectedAt.Add(-time.Hour)
			changed := base.CollectedAt.Add(time.Hour)
			switch name {
			case "first stats reset":
				cur.Window.StatsResetAt = &changed
			case "later stats reset":
				base.Context.Window.StatsResetAt = &old
				cur.Window.StatsResetAt = &changed
			case "postmaster restart":
				base.Context.Window.PostmasterStartAt = &old
				cur.Window.PostmasterStartAt = &changed
			}
			if StatsResetBetween(base.Context, cur) == "" {
				t.Fatal("fixture must contain a detected reset")
			}
			if got := Compute(cur, base, nil); got != nil {
				t.Fatalf("reset-crossing comparison must be unavailable, got %+v", got.Changes)
			}
		})
	}
}

func TestCompute_KeepsComparableSnapshots(t *testing.T) {
	for _, name := range []string{"no reset metadata", "same reset metadata"} {
		t.Run(name, func(t *testing.T) {
			cur, base := resetComparisonFixture()
			if name == "same reset metadata" {
				old := base.CollectedAt.Add(-time.Hour)
				base.Context.Window.StatsResetAt, cur.Window.StatsResetAt = &old, &old
				base.Context.Window.PostmasterStartAt, cur.Window.PostmasterStartAt = &old, &old
			}
			got := Compute(cur, base, nil)
			if got == nil || len(got.Changes) != 1 || got.Changes[0].ID != "query.mean_ms" {
				t.Fatalf("comparable query-mean change was lost: %+v", got)
			}
		})
	}
}

func TestCompute_MissingBaselineRemainsUnavailable(t *testing.T) {
	cur, _ := resetComparisonFixture()
	if Compute(cur, nil, nil) != nil || Compute(cur, &Baseline{}, nil) != nil {
		t.Fatal("missing baseline must remain unavailable")
	}
}
