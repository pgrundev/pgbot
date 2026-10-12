package collect_test

import (
	"context"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/collect"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pggo"
)

// End-to-end wiring guard for the observer-exclusion class, found walking the
// v0.2.0 install path: the collectors must actually route pg_stat_activity
// through ExcludeSelf. Assertions are noise-immune (no exact idle counts) so they
// hold under CI's concurrent write load. The precise by-PID-not-label proof lives
// in the conn package (TestIntegration_excludeSelf_byPIDNotLabel).
func TestIntegration_selfExclusion_wiring(t *testing.T) {
	d := dsn(t)
	ctx := context.Background()

	run := func(target *conn.Target) *model.Context {
		c, err := collect.Run(ctx, target, collect.Options{Interval: 500 * time.Millisecond, ASHHz: 0})
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		if c.Activity == nil {
			t.Fatal("no activity section")
		}
		return c
	}

	target, err := conn.Connect(ctx, d)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer target.Close()

	// The connection breakdown reads pg_stat_activity via ExcludeSelf: pgbot must
	// never list its own pool connections as a connection source.
	if appInConnections(run(target).Activity.Connections, "pgbot") {
		t.Error("pgbot must not list its own connections in the breakdown (conn_breakdown must route through ExcludeSelf)")
	}

	// Hold a real idle-in-transaction session; pgbot must count it. Counting is a
	// lower bound (background activity can only add), so this stays robust while
	// still failing if the activity collector stopped counting real sessions.
	cfg, err := pggo.ParseConfig(d)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.RuntimeParams["application_name"] = "pgbot_selftest_app"
	held, err := pggo.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("hold connect: %v", err)
	}
	defer held.Close()
	if _, err := held.Exec(ctx, "BEGIN READ ONLY"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := held.Exec(ctx, "SELECT 1"); err != nil { // -> idle in transaction
		t.Fatalf("select: %v", err)
	}
	if got := run(target).Activity.IdleInTransaction; got < 1 {
		t.Errorf("a real idle-in-transaction session must be counted, got %d", got)
	}
}

func appInConnections(cs []model.ConnGroup, app string) bool {
	for _, c := range cs {
		if c.AppName == app {
			return true
		}
	}
	return false
}
