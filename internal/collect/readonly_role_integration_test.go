package collect_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/collect"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pggo"
)

// The read-only guarantee's real boundary is the role: a pg_monitor role with no
// write grants. This proves it end to end and self-contained (not relying on CI's
// environment setup): provision exactly such a role, run the WHOLE pipeline as it
// — which passes only if no collector needs write access — and then confirm the
// role itself is denied a write (SQLSTATE 42501). Needs a superuser DSN to create
// the role.
func TestIntegration_readOnlyRole_runsFullPipelineAndCannotWrite(t *testing.T) {
	su := os.Getenv("PGBOT_TEST_SUPERUSER_DSN")
	if su == "" {
		t.Skip("set PGBOT_TEST_SUPERUSER_DSN (a superuser DSN) to run the read-only-role guarantee test")
	}
	ctx := context.Background()

	cfg, err := pggo.ParseConfig(su)
	if err != nil {
		t.Fatalf("parse superuser dsn: %v", err)
	}
	const roUser, roPass = "pgbot_ro_test", "ro_test_pw"

	admin, err := pggo.Connect(ctx, su)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer admin.Close()

	db := pggo.QuoteIdentifier(cfg.Database)
	// Idempotent (re)provision: least privilege — pg_monitor + CONNECT, nothing more.
	_, _ = admin.Exec(ctx, `DROP OWNED BY `+roUser)
	_, _ = admin.Exec(ctx, `DROP ROLE IF EXISTS `+roUser)
	for _, stmt := range []string{
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, roUser, roPass),
		fmt.Sprintf(`GRANT pg_monitor TO %s`, roUser),
		fmt.Sprintf(`GRANT CONNECT ON DATABASE %s TO %s`, db, roUser),
		`DROP TABLE IF EXISTS ro_probe`,
		`CREATE TABLE ro_probe (n int)`,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	defer func() {
		_, _ = admin.Exec(ctx, `DROP TABLE IF EXISTS ro_probe`)
		_, _ = admin.Exec(ctx, `DROP OWNED BY `+roUser)
		_, _ = admin.Exec(ctx, `DROP ROLE IF EXISTS `+roUser)
	}()

	roDSN := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(roUser, roPass),
		Host:     fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Path:     "/" + cfg.Database,
		RawQuery: "sslmode=disable",
	}).String()

	// 1. The whole pipeline must run green as the pg_monitor-only role.
	target, err := conn.Connect(ctx, roDSN)
	if err != nil {
		t.Fatalf("pgbot connect as %s: %v", roUser, err)
	}
	defer target.Close()
	c, err := collect.Run(ctx, target, collect.Options{Interval: 500 * time.Millisecond, ASHHz: 0})
	if err != nil {
		t.Fatalf("full pipeline failed as a read-only role — a collector needs more than pg_monitor: %v", err)
	}
	if !c.Server.HasPgMonitor {
		t.Error("role should hold pg_monitor")
	}
	if c.Health == nil || c.Health.Exactness == model.ExactnessUnavailable {
		t.Error("the pipeline should still produce a health section as a read-only role")
	}

	// 2. The role itself must be unable to write — a raw connection with NO pgbot
	// read-only pinning still cannot INSERT, because the grant simply isn't there.
	raw, err := pggo.Connect(ctx, roDSN)
	if err != nil {
		t.Fatalf("raw connect as %s: %v", roUser, err)
	}
	defer raw.Close()
	_, werr := raw.Exec(ctx, `INSERT INTO ro_probe VALUES (1)`)
	if werr == nil {
		t.Fatal("SAFETY: a pg_monitor role was able to INSERT — it has write access it must not have")
	}
	var pgErr *pggo.PgError
	if !strings.Contains(werr.Error(), "permission denied") &&
		!(errors.As(werr, &pgErr) && pgErr.Code == "42501") {
		t.Errorf("write should be denied for insufficient privilege (42501), got: %v", werr)
	}
}
