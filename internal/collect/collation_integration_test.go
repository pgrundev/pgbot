package collect_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pgrundev/pgbot/internal/collect"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/findings"
	"github.com/pgrundev/pgbot/internal/model"
)

// A collation version mismatch can't be produced by upgrading glibc inside a
// test, but the catalog state it leaves behind can: pg_database.datcollversion
// is what Postgres compares against the library at connect time, and a superuser
// can set it directly. Forge a stale version, run the real collector, check the
// collected row and the finding, then confirm ALTER DATABASE … REFRESH COLLATION
// VERSION — the step the remediation ends with — clears it.
func TestIntegration_collationVersionMismatch(t *testing.T) {
	su := os.Getenv("PGBOT_TEST_SUPERUSER_DSN")
	if su == "" {
		t.Skip("set PGBOT_TEST_SUPERUSER_DSN (a superuser DSN) to run the collation fixture")
	}
	ro := dsn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, su)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })

	var vnum int
	if err := admin.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&vnum); err != nil {
		t.Fatal(err)
	}
	if vnum < 150000 {
		t.Skip("pg_database.datcollversion is PG15+")
	}
	var db string
	var recorded *string
	if err := admin.QueryRow(ctx, `SELECT datname, datcollversion FROM pg_database WHERE datname = current_database()`).
		Scan(&db, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded == nil {
		t.Skip("this database's collation records no version (C/POSIX) — nothing can drift")
	}
	refresh := `ALTER DATABASE ` + pgx.Identifier{db}.Sanitize() + ` REFRESH COLLATION VERSION`
	if _, err := admin.Exec(ctx, `UPDATE pg_database SET datcollversion = '0.0-pgbot-test' WHERE datname = current_database()`); err != nil {
		t.Fatalf("forge a stale datcollversion: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), refresh) })

	target, err := conn.Connect(ctx, ro)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer target.Close()
	run := func() *model.Context {
		c, err := collect.Run(ctx, target, collect.Options{Interval: 200 * time.Millisecond, ASHHz: 0})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if c.Collation == nil || c.Collation.Exactness != model.ExactnessScraped {
			t.Fatalf("collation section must be collected on PG15+, got %+v", c.Collation)
		}
		return c
	}

	c := run()
	var row *model.CollationMismatch
	for i := range c.Collation.Mismatches {
		if c.Collation.Mismatches[i].Kind == "database" {
			row = &c.Collation.Mismatches[i]
		}
	}
	if row == nil {
		t.Fatalf("the forged database mismatch must be collected, got %+v", c.Collation.Mismatches)
	}
	if row.Name != db || row.Recorded != "0.0-pgbot-test" || row.Actual == "" || row.Actual == row.Recorded {
		t.Fatalf("collected row must mirror the catalog: %+v", *row)
	}
	var f *model.Finding
	for _, x := range findings.Compute(c) {
		if x.ID == "collation_version_mismatch" {
			f = &x
			break
		}
	}
	if f == nil || f.Severity != model.SeverityCritical || f.Object != "db:"+db {
		t.Fatalf("a drifted database default must fire critical on db:%s, got %+v", db, f)
	}

	if _, err := admin.Exec(ctx, refresh); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	for _, m := range run().Collation.Mismatches {
		if m.Kind == "database" {
			t.Fatalf("the database mismatch must clear after REFRESH COLLATION VERSION, got %+v", m)
		}
	}
}
