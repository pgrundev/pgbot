package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pgrundev/pgbot/internal/advisor"
	"github.com/pgrundev/pgbot/internal/conn"
)

// TestAdviseSafety_readOnlyTxBlocksInjectedWrite is the belt-and-braces proof for
// the advisor's one raw-SQL surface. sanitizeQuery already rejects any ';' before
// a query reaches the planner (TestSanitizeQuery), so a second statement can never
// be sent — but because GenericPlan uses the simple-query protocol (PgConn.Exec),
// which WOULD execute a ';'-separated second statement, we also prove the READ
// ONLY transaction is a hard backstop: even if a two-statement string reached it,
// the write does not happen. This is what keeps §0.1 ("we never execute the
// inspected query") true rather than merely likely, and it holds under a
// transaction-mode pooler because pgbot opens BEGIN READ ONLY per transaction, not
// a session-level SET the pooler could drop.
func TestIntegration_adviseSafety_readOnlyTxBlocksInjectedWrite(t *testing.T) {
	// Needs a superuser DSN — the setup creates a table the read-only role can't.
	d := os.Getenv("PGBOT_TEST_SUPERUSER_DSN")
	if d == "" {
		t.Skip("set PGBOT_TEST_SUPERUSER_DSN to run the advise-safety integration test")
	}
	ctx := context.Background()

	// A raw connection (NOT pgbot-pinned) to set up and later inspect the table.
	admin, err := pgx.Connect(ctx, d)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, `DROP TABLE IF EXISTS advise_safety; CREATE TABLE advise_safety (n int)`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	target, err := conn.Connect(ctx, d)
	if err != nil {
		t.Fatalf("pgbot connect: %v", err)
	}
	defer target.Close()

	// Feed the planner surface a crafted two-statement string, bypassing
	// sanitizeQuery, exactly as a hostile pgss entry would if the sanitizer missed
	// it. The second statement is a write.
	injected := "EXPLAIN (GENERIC_PLAN, FORMAT JSON) SELECT 1; INSERT INTO advise_safety VALUES (1)"
	_ = target.ReadOnlyTx(ctx, func(tx pgx.Tx) error {
		_, _ = tx.Conn().PgConn().Exec(ctx, injected).ReadAll()
		return nil
	})

	// The write must not have landed — the READ ONLY transaction rejects it.
	var n int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM advise_safety`).Scan(&n); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if n != 0 {
		t.Fatalf("SAFETY VIOLATION: injected write executed (%d rows) — READ ONLY did not hold", n)
	}
	_, _ = admin.Exec(ctx, `DROP TABLE IF EXISTS advise_safety`)
}

func TestIntegration_advisorTargetsResolvedNonPublicSchema(t *testing.T) {
	superuserDSN := os.Getenv("PGBOT_TEST_SUPERUSER_DSN")
	if superuserDSN == "" {
		t.Skip("set PGBOT_TEST_SUPERUSER_DSN to run the advisor schema integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, superuserDSN)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer admin.Close(context.Background())
	var serverVersion int
	if err := admin.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersion); err != nil {
		t.Fatal(err)
	}
	if serverVersion < 160000 {
		t.Skip("generic-plan advisor requires PostgreSQL 16+")
	}
	dbName := fmt.Sprintf("pgbot_advisor_schema_%d", time.Now().UnixNano())
	quotedDB := pgx.Identifier{dbName}.Sanitize()
	dropDatabase := func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, `DROP DATABASE `+quotedDB+` WITH (FORCE)`); err != nil {
			t.Errorf("remove owned fixture database: %v", err)
		}
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+quotedDB); err != nil {
		t.Fatalf("create fixture database: %v", err)
	}
	defer dropDatabase()

	dsn := swapDatabase(t, superuserDSN, dbName)
	fixture, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("fixture connect: %v", err)
	}
	defer fixture.Close(ctx)

	for _, stmt := range []string{
		`CREATE EXTENSION IF NOT EXISTS hypopg`,
		`CREATE SCHEMA pgbot_advise_schema_it`,
		`CREATE TABLE pgbot_advise_schema_it.pgbot_same_name_it (customer_id integer NOT NULL, payload text)`,
		`CREATE TABLE public.pgbot_same_name_it (customer_id integer NOT NULL, payload text)`,
		`INSERT INTO pgbot_advise_schema_it.pgbot_same_name_it SELECT n, repeat('x', 80) FROM generate_series(1, 50000) AS n`,
		`INSERT INTO public.pgbot_same_name_it VALUES (1, 'public control')`,
		`ANALYZE pgbot_advise_schema_it.pgbot_same_name_it`,
		`ANALYZE public.pgbot_same_name_it`,
	} {
		if _, err := fixture.Exec(ctx, stmt); err != nil {
			t.Fatalf("fixture statement failed: %v", err)
		}
	}

	target, err := conn.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgbot connect: %v", err)
	}
	defer target.Close()
	if !target.Caps.HasHypopg {
		t.Fatal("fixture installed hypopg but pgbot did not detect it")
	}

	var recs []advisor.Recommendation
	err = target.ReadOnlyTx(ctx, func(tx pgx.Tx) error {
		planner := pgxPlanner{tx: tx, caps: target.Caps}
		defer func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cleanupCancel()
			_ = planner.ResetHypo(cleanupCtx)
		}()
		recs, _ = advisor.Advise(ctx, planner, []advisor.QueryInput{{
			QueryID: 1,
			Text: "SELECT * FROM pgbot_advise_schema_it.pgbot_same_name_it " +
				"WHERE customer_id = $1",
			Scrubbed: "SELECT * FROM pgbot_advise_schema_it.pgbot_same_name_it " +
				"WHERE customer_id = $1",
			Calls:    100,
			SharePct: 80,
		}}, advisor.Options{MinImprovement: 0.5})
		return nil
	})
	if err != nil {
		t.Fatalf("advisor transaction: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected one non-public recommendation, got %+v", recs)
	}
	const want = "CREATE INDEX ON pgbot_advise_schema_it.pgbot_same_name_it (customer_id)"
	if recs[0].Schema != "pgbot_advise_schema_it" || recs[0].IndexDDL != want {
		t.Fatalf("advisor targeted the wrong same-named relation: %+v", recs[0])
	}
}
