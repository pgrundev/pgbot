package collect

import (
	"context"
	"sync"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
	"golang.org/x/sync/errgroup"
)

// Run performs the two-phase collection and assembles a Context. Gauges and the
// first counter sample are read in phase 1; the second counter sample after the
// interval in phase 2. A single failing collector marks its section unavailable
// rather than failing the run.
//
// The sample window — the slice of wall-clock time every per-second rate
// describes — is opened by the health collector's sample A and closed by its
// sample B, and pgbot keeps its own footprint out of it: every other collector
// runs before A (phase 1) or after B (phase 2), and the only thing that runs
// inside is the wait sampler, whose successful polls are counted and subtracted
// from the commit delta (sampled.OwnTxns). Without that, phase 1's own
// statements (each SET on a fresh connection is a committed implicit
// transaction) and up to hz×window sampler polls were read back as the
// database's TPS — several tps on a quiet server, i.e. the entire figure.
func Run(ctx context.Context, t *conn.Target, opts Options) (*model.Context, error) {
	iv := opts.interval()
	// The active-session sampler (T8) defines the window: it polls
	// pg_stat_activity at ashHz for ashWindow, inside [A, B]. Stretch the
	// counter interval to cover its window so both signals describe the same
	// slice of wall-clock time.
	ashOn := opts.ashHz() > 0
	if ashOn && opts.ashWindow() > iv {
		iv = opts.ashWindow()
	}
	// Total wall-clock cap. The old 5s+iv was fine for a local database but too
	// tight for a remote/large one over the internet (many round trips × latency),
	// and there was no way to extend it. Default generous; callers can override
	// via Options.Deadline (the --timeout flag).
	deadline := opts.Deadline
	if deadline <= 0 {
		deadline = 20*time.Second + iv
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	caps := t.Caps
	var mu sync.Mutex
	results := make(map[string]*sampled, len(registry))

	// The sampler never fails the run — a dead sampler just yields an unavailable
	// profile. It is launched right after sample A (below) so its polls are the
	// only pgbot traffic inside the window.
	var (
		ash     ashResult
		ashDone chan struct{}
	)
	// Guarantee the sampler goroutine has fully exited before Run returns — on
	// EVERY path, including the mid-run cancellation returns below. sampleWaits
	// watches ctx.Done(), so cancel first (idempotent with the deferred cancel
	// above), then wait. Runs before the deferred cancel() (LIFO), so it is the
	// one that actually unblocks the sampler on an early return.
	defer func() {
		if ashDone != nil {
			cancel()
			<-ashDone
		}
	}()

	// Phase 1: sample A for every available collector except health (gauges and
	// the other counters), concurrently and bounded to the pool. This is where
	// pool connections get established (and pinned) — all of it before the window.
	g1, gctx := errgroup.WithContext(ctx)
	g1.SetLimit(4)
	for _, c := range registry {
		c := c
		if c.Name() == healthName {
			continue
		}
		if !c.Available(caps) {
			mu.Lock()
			results[c.Name()] = &sampled{}
			mu.Unlock()
			continue
		}
		g1.Go(func() error {
			v, err := c.Sample(gctx, t, caps)
			mu.Lock()
			results[c.Name()] = &sampled{A: v, Err: err}
			mu.Unlock()
			return nil
		})
	}
	_ = g1.Wait()

	// Sample A opens the window.
	health := healthCollector{}
	hA, hErr := health.Sample(ctx, t, caps)
	tA := nowUTC()
	if ashOn {
		ashDone = make(chan struct{})
		go func() {
			defer close(ashDone)
			ash = sampleWaits(ctx, t, caps, opts.ashHz(), opts.ashWindow())
		}()
	}

	// Wait out the sample interval, let an in-flight poll finish, then close the
	// window with sample B — so every counted poll is inside [A, B].
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(iv):
	}
	if ashOn {
		<-ashDone
	}
	hB, hErrB := health.Sample(ctx, t, caps)
	tB := nowUTC()
	dt := tB.Sub(tA)
	if hErr == nil {
		hErr = hErrB
	}
	results[healthName] = &sampled{A: hA, B: hB, Err: hErr, OwnTxns: int64(ash.attempts - ash.failures)}

	// Phase 2: the second sample for the remaining counters — after the window.
	g2, gctx2 := errgroup.WithContext(ctx)
	g2.SetLimit(4)
	for _, c := range registry {
		c := c
		if c.Kind() != KindCounter || c.Name() == healthName || !c.Available(caps) {
			continue
		}
		g2.Go(func() error {
			v, err := c.Sample(gctx2, t, caps)
			mu.Lock()
			r := results[c.Name()]
			if r == nil {
				r = &sampled{}
				results[c.Name()] = r
			}
			r.B = v
			if err != nil && r.Err == nil {
				r.Err = err
			}
			mu.Unlock()
			return nil
		})
	}
	_ = g2.Wait()

	out := newContext(caps, tB, dt)
	for _, c := range registry {
		s := results[c.Name()]
		if s == nil {
			s = &sampled{}
		}
		c.Assemble(out, caps, *s, dt, opts)
	}

	// Fold in the active-session profile. Query text for per-query attribution
	// comes from the queries collector's already-scrubbed normals.
	if ashOn {
		out.WaitProfile = profileFrom(ash, queryTexts(out))
	} else {
		out.WaitProfile = &model.WaitProfile{Available: false, Reason: model.WaitSamplerDisabledReason}
	}
	return out, nil
}

// queryTexts maps query_id → scrubbed normalized text from the queries
// collector, for best-effort per-query attribution in the wait profile.
func queryTexts(c *model.Context) map[int64]string {
	if c.Queries == nil {
		return nil
	}
	m := make(map[int64]string, len(c.Queries.Top))
	for _, q := range c.Queries.Top {
		m[q.QueryID] = q.Query
	}
	return m
}
