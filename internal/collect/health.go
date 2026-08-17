package collect

import (
	"context"
	_ "embed"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/rate"
)

//go:embed sql/health.sql
var sqlHealth string

// health = pg_stat_database counters, double-sampled for aggregate rates.
type healthCollector struct{}

type healthSample struct {
	Numbackends  int32      `db:"numbackends"`
	XactCommit   int64      `db:"xact_commit"`
	XactRollback int64      `db:"xact_rollback"`
	BlksRead     int64      `db:"blks_read"`
	BlksHit      int64      `db:"blks_hit"`
	TupReturned  int64      `db:"tup_returned"`
	TupFetched   int64      `db:"tup_fetched"`
	TupInserted  int64      `db:"tup_inserted"`
	TupUpdated   int64      `db:"tup_updated"`
	TupDeleted   int64      `db:"tup_deleted"`
	Deadlocks    int64      `db:"deadlocks"`
	TempBytes    int64      `db:"temp_bytes"`
	StatsReset   *time.Time `db:"stats_reset"`
}

// healthName is the registry name the runner uses to sequence this collector
// around the sample window (see Run).
const healthName = "health"

func (healthCollector) Name() string                     { return healthName }
func (healthCollector) Kind() Kind                       { return KindCounter }
func (healthCollector) Available(conn.Capabilities) bool { return true }

func (healthCollector) Sample(ctx context.Context, t *conn.Target, _ conn.Capabilities) (any, error) {
	// A single-row read on an already-read-only session — run it directly on the
	// pool (one round trip) rather than wrapping it in BEGIN/COMMIT (three). The
	// window is bracketed by two of these serially (runner.go), so the round
	// trips are on the critical path; the saved transactions are also two fewer
	// of pgbot's own commits per run.
	rows, err := t.Pool.Query(ctx, sqlHealth)
	if err != nil {
		return healthSample{}, err
	}
	return pgx.CollectExactlyOneRow(rows, pgx.RowToStructByNameLax[healthSample])
}

func (healthCollector) Assemble(c *model.Context, _ conn.Capabilities, s sampled, dt time.Duration, _ Options) {
	a, aok := s.A.(healthSample)
	b, bok := s.B.(healthSample)
	if s.Err != nil || !aok || !bok {
		c.Health = &model.Health{Section: unavail(s.Err, "pg_stat_database unavailable")}
		return
	}
	h := &model.Health{Connections: int(b.Numbackends)}
	reset := false
	mark := func(v *float64, ok bool) *float64 {
		if !ok {
			reset = true
			return nil
		}
		return v
	}
	// pgbot's own commits inside the window (the wait sampler's polls) are not
	// the database's throughput: take them off sample B's commit counter before
	// computing rates. Clamped so a reset (b < a) is still detected as such.
	commitsB := b.XactCommit
	if own := s.OwnTxns; own > 0 && commitsB-own >= a.XactCommit {
		commitsB -= own
	}
	h.TPS = mark(rate.PerSecond(a.XactCommit+a.XactRollback, commitsB+b.XactRollback, dt))
	h.CommitsPerSec = mark(rate.PerSecond(a.XactCommit, commitsB, dt))
	h.RollbacksPerSec = mark(rate.PerSecond(a.XactRollback, b.XactRollback, dt))
	if rr, ok := rate.Ratio(a.XactRollback, b.XactRollback, a.XactCommit, commitsB); ok {
		h.RollbackRatio = round4p(rr)
	}
	if chr, ok := rate.Ratio(a.BlksHit, b.BlksHit, a.BlksRead, b.BlksRead); ok {
		h.CacheHitRatio = round4p(chr)
		blocks := (b.BlksHit + b.BlksRead) - (a.BlksHit + a.BlksRead)
		h.CacheBlocks = &blocks
	}
	if d, ok := rate.PerSecond(a.Deadlocks, b.Deadlocks, dt); ok {
		h.DeadlocksPerMin = rate.Ptr(round2(*d * 60))
	}
	if tb, ok := rate.PerSecond(a.TempBytes, b.TempBytes, dt); ok {
		h.TempBytesPerSec = round2p(tb)
	}
	if tr, ok := rate.PerSecond(a.TupReturned, b.TupReturned, dt); ok {
		h.TupReturnedPerS = round2p(tr)
	}
	if tw, ok := rate.PerSecond(a.TupInserted+a.TupUpdated+a.TupDeleted, b.TupInserted+b.TupUpdated+b.TupDeleted, dt); ok {
		h.TupWrittenPerS = round2p(tw)
	}
	if reset {
		h.Section = model.Section{Exactness: model.ExactnessReset, Reason: "a counter reset between samples; rates omitted"}
	} else {
		h.Section = model.Section{Exactness: model.ExactnessSampled}
	}
	setStatsWindow(c, b.StatsReset)
	c.Health = h
}

// setStatsWindow records the stats-reset age on the window (health is where we
// learn it). T2 uses these fields for cold-start / reset detection. stats_reset
// is the effective start of the cumulative window, so it overrides the
// provisional (uptime-based) age set in newContext.
func setStatsWindow(c *model.Context, statsReset *time.Time) {
	if statsReset == nil {
		return
	}
	c.Window.StatsResetAt = statsReset
	days := c.CollectedAt.Sub(*statsReset).Hours() / 24
	c.Window.StatsWindowDays = round2p(rate.Ptr(days))
	age := int64(c.CollectedAt.Sub(*statsReset).Seconds())
	c.Window.WindowAgeSeconds = &age
}
