package conn

import (
	"context"
	"os"
	"testing"

	"github.com/pgrundev/pggo"
)

// Direct PostgreSQL must never be identified as PgDog (issue #22): the probe's
// SET LOCAL pgdog.shard lands on a real backend as a placeholder GUC and echoes
// back, so the "consumed" verdict is impossible. The test also proves the probe
// leaves no residue — the placeholder dies with its transaction.
func TestIntegration_detectPgDog_notOnRealPostgres(t *testing.T) {
	d := os.Getenv("PGBOT_TEST_DSN")
	if d == "" {
		t.Skip("set PGBOT_TEST_DSN to run integration tests")
	}
	ctx := context.Background()
	cfg, err := pggo.ParseConfig(d)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	c, err := pggo.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()

	if detectPgDog(ctx, c) {
		t.Error("real PostgreSQL misidentified as PgDog")
	}

	// The SET LOCAL must not have leaked past its transaction.
	var got *string
	if err := c.QueryRow(ctx, "SELECT current_setting('pgdog.shard', true)").Scan(&got); err != nil {
		t.Fatalf("readback after probe: %v", err)
	}
	if got != nil && *got != "" {
		t.Errorf("probe leaked pgdog.shard=%q into the session", *got)
	}

	// And the full detector must still work end to end on this connection.
	info := detectPooler(ctx, c, cfg)
	if info.Hint == "a PgDog pooler" {
		t.Errorf("detectPooler labeled a direct connection as PgDog: %+v", info)
	}
}

// The positive path needs a real PgDog in front of a real Postgres, so it has
// its own opt-in DSN (e.g. a local `docker run ghcr.io/pgdogdev/pgdog`).
func TestIntegration_detectPgDog_throughPgDog(t *testing.T) {
	d := os.Getenv("PGBOT_PGDOG_TEST_DSN")
	if d == "" {
		t.Skip("set PGBOT_PGDOG_TEST_DSN (a DSN through a PgDog pooler) to run")
	}
	ctx := context.Background()
	cfg, err := pggo.ParseConfig(d)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	c, err := pggo.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()

	if !detectPgDog(ctx, c) {
		t.Error("PgDog not identified by the behavioral probe")
	}
	info := detectPooler(ctx, c, cfg)
	if !info.Detected || info.Hint != "a PgDog pooler" {
		t.Errorf("detectPooler through PgDog = %+v, want Detected with the PgDog hint", info)
	}
}
