package conn

import (
	"context"
	"os"
	"testing"

	"github.com/pgrundev/pggo"
)

// Self-exclusion must key on backend PID, not the 'pgbot' application_name: a
// stranger's session that sets application_name='pgbot' stays visible
// (unspoofable), while every one of pgbot's own pool backends is dropped. The
// test checks specific PIDs, so it is immune to any other activity on the
// database (e.g. CI's concurrent write load).
func TestIntegration_excludeSelf_byPIDNotLabel(t *testing.T) {
	d := os.Getenv("PGBOT_TEST_DSN")
	if d == "" {
		t.Skip("set PGBOT_TEST_DSN to run integration tests")
	}
	ctx := context.Background()
	target, err := Connect(ctx, d)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer target.Close()

	target.Warm(ctx) // register every one of our own backend PIDs up front
	own := target.self.list()
	if len(own) == 0 {
		t.Fatal("pool warmed but no own PIDs were captured")
	}

	// An impostor: an external connection labelled application_name='pgbot'.
	cfg, err := pggo.ParseConfig(d)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.RuntimeParams["application_name"] = "pgbot"
	imp, err := pggo.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("impostor connect: %v", err)
	}
	defer imp.Close()
	var impPID uint32
	if err := imp.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&impPID); err != nil {
		t.Fatalf("impostor pid: %v", err)
	}

	// Run the exact predicate the collectors use.
	sql := target.ExcludeSelf("SELECT pid FROM pg_stat_activity WHERE pid <> pg_backend_pid()")
	rows, err := target.Pool.Query(ctx, sql)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	visible := map[uint32]bool{}
	for rows.Next() {
		var pid uint32
		if err := rows.Scan(&pid); err != nil {
			t.Fatal(err)
		}
		visible[pid] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if !visible[impPID] {
		t.Errorf("external session labelled application_name='pgbot' (pid %d) must stay visible — exclude by PID, not label", impPID)
	}
	for _, p := range own {
		if visible[p] {
			t.Errorf("pgbot's own backend pid %d must be excluded", p)
		}
	}
}
