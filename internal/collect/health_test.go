package collect

import (
	"math"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
)

func TestHealthAssemble_resetEpochOmitsEveryDerivedCounter(t *testing.T) {
	resetA := time.Date(2026, 9, 22, 1, 0, 0, 0, time.UTC)
	resetB := resetA.Add(time.Minute)
	a, b := healthTrafficSamples(&resetA, &resetB)
	c := &model.Context{CollectedAt: resetB.Add(48 * time.Hour)}

	healthCollector{}.Assemble(c, conn.Capabilities{}, sampled{A: a, B: b}, 2*time.Second, Options{})

	assertHealthReset(t, c, 7)
	if c.Window.StatsResetAt == nil || !c.Window.StatsResetAt.Equal(resetB) {
		t.Fatalf("stats window must use sample B's epoch: %+v", c.Window)
	}
	if c.Window.StatsWindowDays == nil || *c.Window.StatsWindowDays != 2 {
		t.Fatalf("stats-window days = %v, want 2", c.Window.StatsWindowDays)
	}
	if c.Window.WindowAgeSeconds == nil || *c.Window.WindowAgeSeconds != int64((48*time.Hour)/time.Second) {
		t.Fatalf("stats-window age = %v, want 172800", c.Window.WindowAgeSeconds)
	}
}

func TestHealthAssemble_epochBoundaries(t *testing.T) {
	t0 := time.Date(2026, 9, 22, 1, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	tBackwards := t0.Add(-time.Hour)

	for _, tc := range []struct {
		name      string
		before    *time.Time
		after     *time.Time
		wantReset bool
	}{
		{name: "nil to known epoch", before: nil, after: &t1, wantReset: true},
		{name: "unchanged epoch", before: &t0, after: &t0, wantReset: false},
		{name: "backwards reset clock still changes epoch", before: &t0, after: &tBackwards, wantReset: true},
		{name: "both epochs unknown", before: nil, after: nil, wantReset: false},
		{name: "known epoch becomes unknown", before: &t0, after: nil, wantReset: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := healthTrafficSamples(tc.before, tc.after)
			c := &model.Context{CollectedAt: t1.Add(time.Hour)}

			healthCollector{}.Assemble(c, conn.Capabilities{}, sampled{A: a, B: b}, 2*time.Second, Options{})

			if tc.wantReset {
				assertHealthReset(t, c, 7)
				return
			}
			if c.Health.Exactness != model.ExactnessSampled || c.Health.TPS == nil {
				t.Fatalf("same or unknown epochs with monotonic counters must remain sampled: %+v", c.Health)
			}
		})
	}
}

func TestHealthAssemble_anyNegativeCounterDeltaInvalidatesTheEpoch(t *testing.T) {
	reset := time.Date(2026, 9, 22, 1, 0, 0, 0, time.UTC)
	mutations := map[string]func(*healthSample){
		"commits":         func(b *healthSample) { b.XactCommit = 99 },
		"rollbacks":       func(b *healthSample) { b.XactRollback = 9 },
		"blocks read":     func(b *healthSample) { b.BlksRead = 99 },
		"blocks hit":      func(b *healthSample) { b.BlksHit = 899 },
		"tuples returned": func(b *healthSample) { b.TupReturned = 99 },
		"tuples inserted": func(b *healthSample) { b.TupInserted = 19 },
		"tuples updated":  func(b *healthSample) { b.TupUpdated = 29 },
		"tuples deleted":  func(b *healthSample) { b.TupDeleted = 39 },
		"deadlocks":       func(b *healthSample) { b.Deadlocks = 0 },
		"temp bytes":      func(b *healthSample) { b.TempBytes = 99 },
	}

	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			a, b := healthTrafficSamples(&reset, &reset)
			mutate(&b)
			c := &model.Context{CollectedAt: reset.Add(time.Hour)}

			healthCollector{}.Assemble(c, conn.Capabilities{}, sampled{A: a, B: b}, 2*time.Second, Options{})

			assertHealthReset(t, c, 7)
		})
	}
}

func TestHealthAssemble_noResetTrafficKeepsRatesAndGauges(t *testing.T) {
	reset := time.Date(2026, 9, 20, 1, 0, 0, 0, time.UTC)
	a, b := healthTrafficSamples(&reset, &reset)
	c := &model.Context{CollectedAt: reset.Add(48 * time.Hour)}

	healthCollector{}.Assemble(c, conn.Capabilities{}, sampled{A: a, B: b, OwnTxns: 4}, 2*time.Second, Options{})

	h := c.Health
	if h.Exactness != model.ExactnessSampled || h.Connections != 7 {
		t.Fatalf("ordinary traffic must remain sampled with the B gauge: %+v", h)
	}
	assertFloat(t, "tps", h.TPS, 12)
	assertFloat(t, "commits", h.CommitsPerSec, 10)
	assertFloat(t, "rollbacks", h.RollbacksPerSec, 2)
	assertFloat(t, "rollback ratio", h.RollbackRatio, 1.0/6.0)
	assertFloat(t, "cache hit", h.CacheHitRatio, 0.9)
	if h.CacheBlocks == nil || *h.CacheBlocks != 200 {
		t.Fatalf("cache blocks = %v, want 200", h.CacheBlocks)
	}
	assertFloat(t, "deadlocks/min", h.DeadlocksPerMin, 60)
	assertFloat(t, "temp bytes/s", h.TempBytesPerSec, 200)
	assertFloat(t, "tuples returned/s", h.TupReturnedPerS, 200)
	assertFloat(t, "tuples written/s", h.TupWrittenPerS, 30)
	if c.Window.StatsResetAt == nil || !c.Window.StatsResetAt.Equal(reset) ||
		c.Window.StatsWindowDays == nil || *c.Window.StatsWindowDays != 2 ||
		c.Window.WindowAgeSeconds == nil || *c.Window.WindowAgeSeconds != int64((48*time.Hour)/time.Second) {
		t.Fatalf("ordinary traffic must retain the stats-window gauges: %+v", c.Window)
	}
}

func healthTrafficSamples(resetA, resetB *time.Time) (healthSample, healthSample) {
	a := healthSample{
		Numbackends: 3, XactCommit: 100, XactRollback: 10,
		BlksRead: 100, BlksHit: 900, TupReturned: 100,
		TupInserted: 20, TupUpdated: 30, TupDeleted: 40,
		Deadlocks: 1, TempBytes: 100, StatsReset: resetA,
	}
	b := healthSample{
		Numbackends: 7, XactCommit: 124, XactRollback: 14,
		BlksRead: 120, BlksHit: 1080, TupReturned: 500,
		TupInserted: 30, TupUpdated: 50, TupDeleted: 70,
		Deadlocks: 3, TempBytes: 500, StatsReset: resetB,
	}
	return a, b
}

func assertHealthReset(t *testing.T, c *model.Context, wantConnections int) {
	t.Helper()
	h := c.Health
	if h == nil || h.Exactness != model.ExactnessReset {
		t.Fatalf("health exactness = %+v, want %q", h, model.ExactnessReset)
	}
	if h.Connections != wantConnections {
		t.Fatalf("point-in-time connections = %d, want %d", h.Connections, wantConnections)
	}
	if h.TPS != nil || h.CommitsPerSec != nil || h.RollbacksPerSec != nil ||
		h.RollbackRatio != nil || h.CacheHitRatio != nil || h.CacheBlocks != nil ||
		h.DeadlocksPerMin != nil || h.TempBytesPerSec != nil ||
		h.TupReturnedPerS != nil || h.TupWrittenPerS != nil {
		t.Fatalf("a crossed counter epoch must omit every derived field: %+v", h)
	}
}

func assertFloat(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 0.0001 {
		t.Fatalf("%s = %v, want %.6f", name, got, want)
	}
}
