package main

import (
	"context"
	"os"
	"testing"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pggo"
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
	admin, err := pggo.Connect(ctx, d)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer admin.Close()
	if _, err := admin.SimpleQuery(ctx, `DROP TABLE IF EXISTS advise_safety; CREATE TABLE advise_safety (n int)`); err != nil {
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
	_ = target.ReadOnlyTx(ctx, func(tx *pggo.Tx) error {
		_, _ = tx.Conn().SimpleQuery(ctx, injected)
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
