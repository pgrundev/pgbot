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
func Run(ctx context.Context, t *conn.Target, opts Options) (*model.Context, error) {
	iv := opts.interval()
	// The active-session sampler (T8) defines the window: it runs concurrently
	// with the whole two-phase counter collection, polling pg_stat_activity at
	// ashHz for ashWindow. Stretch the counter interval to cover its window so
	// both signals describe the same slice of wall-clock time.
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
	// profile. Launch it before phase 1 so its window brackets the counters.
	var (
		ash     ashResult
		ashDone chan struct{}
	)
	if ashOn {
		ashDone = make(chan struct{})
		go func() {
			defer close(ashDone)
			ash = sampleWaits(ctx, t, caps, opts.ashHz(), opts.ashWindow())
		}()
	}
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

	// Phase 1: sample A for every available collector (counters and gauges),
	// concurrently and bounded to the pool.
	tA := nowUTC()
	g1, gctx := errgroup.WithContext(ctx)
	g1.SetLimit(4)
	for _, c := range registry {
		c := c
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

	// Wait out the sample interval, then re-read the counters (sample B).
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(iv):
	}
	tB := nowUTC()
	dt := tB.Sub(tA)

	g2, gctx2 := errgroup.WithContext(ctx)
	g2.SetLimit(4)
	for _, c := range registry {
		c := c
		if c.Kind() != KindCounter || !c.Available(caps) {
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

	// Fold in the active-session profile once the sampler has finished its window
	// (it started before phase 1, so this rarely blocks). Query text for per-query
	// attribution comes from the queries collector's already-scrubbed normals.
	if ashOn {
		<-ashDone
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
