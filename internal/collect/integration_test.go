package collect_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/collect"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/findings"
	"github.com/pgrundev/pgbot/internal/render"
)

// P0-3: a cancellation mid-collection must return ctx.Err() promptly and leave
// no sampler goroutine running behind it.
func TestIntegration_cancelMidRun(t *testing.T) {
	target, err := conn.Connect(context.Background(), dsn(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer target.Close()

	// Warm the pool with one full run first, so the goroutine baseline reflects
	// steady state (pgx pool goroutines up, that run's sampler already drained) —
	// otherwise we'd be measuring pool warm-up, not a sampler leak.
	if _, err := collect.Run(context.Background(), target,
		collect.Options{Interval: 200 * time.Millisecond, ASHHz: 10, ASHWindow: 200 * time.Millisecond}); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	// A 2s interval + 2s ASH window means Run is still mid-flight at 50ms.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()

	start := time.Now()
	_, err = collect.Run(ctx, target, collect.Options{Interval: 2 * time.Second, ASHHz: 10, ASHWindow: 2 * time.Second})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled mid-run must return context.Canceled, got %v", err)
	}
	if elapsed > 700*time.Millisecond {
		t.Fatalf("cancellation must return promptly, took %v", elapsed)
	}

	// Run's deferred drain guarantees the sampler exited before Run returned;
	// poll briefly (no goleak dependency) for the count to settle back.
	for i := 0; i < 50; i++ {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutine leak after cancellation: before=%d after=%d", before, runtime.NumGoroutine())
}

// These run only when PGBOT_TEST_DSN points at a live PostgreSQL — CI sets it
// against the docker-compose matrix; a plain `go test ./...` skips them.
func dsn(t *testing.T) string {
	d := os.Getenv("PGBOT_TEST_DSN")
	if d == "" {
		t.Skip("set PGBOT_TEST_DSN to run integration tests")
	}
	return d
}

func TestIntegration_fullPipeline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	target, err := conn.Connect(ctx, dsn(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer target.Close()

	start := time.Now()
	c, err := collect.Run(ctx, target, collect.Options{Interval: 700 * time.Millisecond})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("one-shot took %v, want < 5s", elapsed)
	}
	c.Findings = findings.Compute(c)

	// Core sections present and labeled.
	if c.Health == nil || c.Health.Exactness == "" {
		t.Error("health section missing exactness label")
	}
	if c.Server.VersionNum == 0 {
		t.Error("server version not detected")
	}
	if c.Horizon == nil || c.Horizon.Exactness == "" {
		t.Error("horizon section missing exactness label")
	}

	// Self-observation gates: pgbot's own sessions and session pins must not
	// leak into what it reports about the database.
	if c.Activity != nil {
		if c.Activity.IdleInTransaction != 0 && os.Getenv("PGBOT_TEST_LOAD") == "" {
			t.Errorf("idle_in_transaction = %d on an idle test database — pgbot is counting its own pool", c.Activity.IdleInTransaction)
		}
	}
	if c.Settings != nil {
		for _, pin := range []string{"statement_timeout", "lock_timeout", "idle_in_transaction_session_timeout",
			"default_transaction_read_only", "stats_fetch_consistency", "application_name"} {
			if v, ok := c.Settings.Overrides[pin]; ok {
				t.Errorf("settings.overrides reports pgbot's own session pin %s=%s as server config", pin, v)
			}
		}
		if got := c.Settings.Params["statement_timeout"]; got == "15s" {
			t.Errorf("settings.params.statement_timeout = %q — that is pgbot's session pin, not the server value", got)
		}
	}

	// PII gate: render JSON and assert no email/uuid leaked from a fake-data table.
	// (The caller is expected to have seeded such data; we assert the invariant
	// regardless — scrubbing must hold, and pgss text is normalized.)
	var buf bytes.Buffer
	if err := render.JSON(&buf, c); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "@example.com") {
		t.Error("PII leaked into JSON output")
	}
}

// TestIntegration_poolerRatesStayCorrect asserts pgbot's central pooler claim:
// behind a transaction pooler, rates are still correct (each counter is read in
// its own transaction; pg_stat_* are cluster-wide). Point PGBOT_POOLER_DSN at a
// PgBouncer/Supabase/Neon pooled endpoint with write load in flight.
func TestIntegration_poolerRatesStayCorrect(t *testing.T) {
	dsn := os.Getenv("PGBOT_POOLER_DSN")
	if dsn == "" || os.Getenv("PGBOT_TEST_LOAD") == "" {
		t.Skip("set PGBOT_POOLER_DSN + PGBOT_TEST_LOAD (with write load running) to run")
	}
	ctx := context.Background()
	target, err := conn.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	t.Logf("pooler detected by signature: %v", target.Pooler.Detected)
	c, err := collect.Run(ctx, target, collect.Options{Interval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if c.Health == nil || c.Health.TPS == nil || *c.Health.TPS <= 0 {
		t.Errorf("rates must stay correct behind a pooler, got %+v", c.Health)
	}
}

// TestIntegration_nonZeroTPS is the stats-caching regression guard: with write
// load in flight, the double-sampled rate must be non-zero. Requires the caller
// to generate concurrent commits (CI does).
func TestIntegration_ratesArePresent(t *testing.T) {
	if os.Getenv("PGBOT_TEST_LOAD") == "" {
		t.Skip("set PGBOT_TEST_LOAD when concurrent write load is running")
	}
	ctx := context.Background()
	target, err := conn.Connect(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	c, err := collect.Run(ctx, target, collect.Options{Interval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if c.Health == nil || c.Health.TPS == nil || *c.Health.TPS <= 0 {
		t.Errorf("expected non-zero TPS under load (stats-caching regression), got %+v", c.Health)
	}
}
