package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/collect"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pggo"
)

// TestIntegration_schemaProfile_acceptance is the D3 acceptance test (DoD 9 & 10):
// a sound, freshly-migrated (empty, never-queried) schema produces ZERO
// findings under --profile=schema, and adding one unindexed foreign key produces
// exactly one new finding that fails the check. Needs a superuser DSN to create
// the fixture database.
func TestIntegration_schemaProfile_acceptance(t *testing.T) {
	su := os.Getenv("PGBOT_TEST_SUPERUSER_DSN")
	if su == "" {
		t.Skip("set PGBOT_TEST_SUPERUSER_DSN to run the schema-profile acceptance test")
	}
	ctx := context.Background()
	const dbName = "pgbot_d3_accept"

	admin, err := pggo.Connect(ctx, su)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer admin.Close()
	_, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+dbName+` WITH (FORCE)`)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+dbName); err != nil {
		t.Fatalf("create db: %v", err)
	}
	defer func() { _, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+dbName+` WITH (FORCE)`) }()

	dsn := swapDatabase(t, su, dbName)

	// A sound schema: bigint keys, the FK is indexed, no invalid/redundant indexes.
	fixture, err := pggo.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("fixture connect: %v", err)
	}
	defer fixture.Close()
	mustExec(t, ctx, fixture,
		`CREATE TABLE users (id bigserial PRIMARY KEY, email text)`,
		`CREATE TABLE orders (id bigserial PRIMARY KEY, user_id bigint REFERENCES users(id), total numeric)`,
		`CREATE INDEX ON orders(user_id)`,
	)

	schemaFindings := func() []model.Finding {
		target, err := conn.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("pgbot connect: %v", err)
		}
		defer target.Close()
		c, err := collect.Run(ctx, target, collect.Options{SchemaOnly: true, ASHHz: 0})
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		if err := computeFindings(c, inspectFlags{profile: "schema", noStore: true}); err != nil {
			t.Fatalf("computeFindings: %v", err)
		}
		return c.Findings
	}

	// DoD 9: a sound schema is silent.
	if fs := schemaFindings(); len(fs) != 0 {
		t.Fatalf("DoD 9: a sound schema must produce zero findings, got %d: %v", len(fs), ids(fs))
	}

	// DoD 10: one unindexed FK → exactly one finding, and it fails --fail-on=warn.
	mustExec(t, ctx, fixture, `CREATE TABLE items (id bigserial PRIMARY KEY, order_id bigint REFERENCES orders(id))`)
	fs := schemaFindings()
	if len(fs) != 1 || fs[0].ID != "fk_unindexed" {
		t.Fatalf("DoD 10: one unindexed FK must produce exactly one fk_unindexed finding, got %v", ids(fs))
	}
	if exitCode(fs, "warn") == 0 {
		t.Error("DoD 10: the introduced finding must fail --fail-on=warn")
	}
}

func swapDatabase(t *testing.T, dsn, db string) string {
	t.Helper()
	cfg, err := pggo.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(cfg.User, cfg.Password),
		Host:     fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Path:     "/" + db,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

func mustExec(t *testing.T, ctx context.Context, c *pggo.Conn, stmts ...string) {
	t.Helper()
	tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for _, s := range stmts {
		if _, err := c.Exec(tctx, s); err != nil {
			t.Fatalf("exec %q: %v", s, err)
		}
	}
}

func ids(fs []model.Finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.ID
	}
	return out
}
