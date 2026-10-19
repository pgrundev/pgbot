package plan

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
)

// Runs only when PGBOT_TEST_DSN points at a live PostgreSQL (CI's docker matrix).
func testTarget(t *testing.T) (*conn.Target, context.Context) {
	t.Helper()
	dsn := os.Getenv("PGBOT_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGBOT_TEST_DSN to run integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	target, err := conn.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(target.Close)
	return target, ctx
}

func TestIntegration_fetchReturnsAParseablePlan(t *testing.T) {
	target, ctx := testTarget(t)
	js, generic, err := Fetch(ctx, target, "SELECT relname FROM pg_class WHERE relkind = 'r' ORDER BY relname")
	if err != nil {
		t.Fatal(err)
	}
	if generic != (target.Caps.VersionNum >= 160000) {
		t.Errorf("generic = %v on PG %d", generic, target.Caps.VersionNum)
	}
	root, err := Parse(js)
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, js)
	}
	if root.NodeType == "" || root.TotalCost <= 0 {
		t.Errorf("root = %+v", root)
	}
	if root.Gloss() == "" {
		t.Error("empty gloss")
	}
}

func TestIntegration_placeholdersPlanGenericallyOn16Plus(t *testing.T) {
	target, ctx := testTarget(t)
	if target.Caps.VersionNum < 160000 {
		t.Skip("GENERIC_PLAN needs PostgreSQL 16+")
	}
	js, _, err := Fetch(ctx, target, "SELECT * FROM pg_class WHERE oid = $1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(js); err != nil {
		t.Fatal(err)
	}
}

// The query must be planned, not run: a pg_sleep(30) that executed would blow
// the deadline; planned, it returns at once.
func TestIntegration_queryIsNeverExecuted(t *testing.T) {
	target, ctx := testTarget(t)
	start := time.Now()
	if _, _, err := Fetch(ctx, target, "SELECT pg_sleep(30)"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("EXPLAIN took %s — the query ran", d)
	}
}

func TestIntegration_writesAreRefusedBeforeReachingTheServer(t *testing.T) {
	target, ctx := testTarget(t)
	for _, q := range []string{"DELETE FROM pg_class", "SELECT 1; DELETE FROM pg_class", "WITH d AS (DELETE FROM pg_class RETURNING 1) SELECT * FROM d"} {
		if _, _, err := Fetch(ctx, target, q); err == nil {
			t.Errorf("%q accepted", q)
		}
	}
}
