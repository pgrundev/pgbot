package collect_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/collect"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/findings"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pggo"
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
	admin, err := pggo.Connect(ctx, su)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

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
	refresh := `ALTER DATABASE ` + pggo.QuoteIdentifier(db) + ` REFRESH COLLATION VERSION`
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

// #130: forge drift on a new ICU collation and on the pinned built-in "unicode",
// which gets no pg_depend rows, then check each kind of use is detected.
func TestIntegration_collationReferenced(t *testing.T) {
	su := os.Getenv("PGBOT_TEST_SUPERUSER_DSN")
	if su == "" {
		t.Skip("set PGBOT_TEST_SUPERUSER_DSN (a superuser DSN) to run the collation fixture")
	}
	ro := dsn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := pggo.Connect(ctx, su)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	var vnum int
	var icu bool
	if err := admin.QueryRow(ctx, `SELECT current_setting('server_version_num')::int,
		EXISTS (SELECT 1 FROM pg_collation WHERE collname = 'unicode' AND collprovider = 'i')`).Scan(&vnum, &icu); err != nil {
		t.Fatal(err)
	}
	if vnum < 160000 || !icu {
		t.Skip("needs PG16+ built with ICU (the built-in unicode collation)")
	}
	exec := func(sql string) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		for _, s := range []string{
			`DROP TABLE IF EXISTS pgbot_coll_col, pgbot_coll_part, pgbot_coll_unicode`,
			`DROP COLLATION IF EXISTS pgbot_coll_test`,
			`ALTER COLLATION pg_catalog."unicode" REFRESH VERSION`,
		} {
			_, _ = admin.Exec(context.Background(), s)
		}
	})
	exec(`CREATE COLLATION pgbot_coll_test (provider = icu, locale = 'de-u-co-phonebk')`)
	exec(`UPDATE pg_collation SET collversion = '0.0-pgbot-test'
	      WHERE (collnamespace, collname) IN (('public'::regnamespace, 'pgbot_coll_test'), ('pg_catalog'::regnamespace, 'unicode'))`)

	target, err := conn.Connect(ctx, ro)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer target.Close()
	check := func(stage string, wantTest, wantUnicode bool, wantSev string) {
		t.Helper()
		c, err := collect.Run(ctx, target, collect.Options{Interval: 200 * time.Millisecond, ASHHz: 0})
		if err != nil {
			t.Fatalf("%s: run: %v", stage, err)
		}
		got := map[string]bool{}
		for _, m := range c.Collation.Mismatches {
			got[m.Name] = m.Unreferenced
		}
		for name, want := range map[string]bool{"public.pgbot_coll_test": wantTest, "pg_catalog.unicode": wantUnicode} {
			u, ok := got[name]
			if !ok {
				t.Fatalf("%s: forged drift on %s not collected: %v", stage, name, got)
			}
			if u != want {
				t.Errorf("%s: %s unreferenced=%v, want %v", stage, name, u, want)
			}
		}
		for _, f := range findings.Compute(c) {
			if f.ID == "collation_version_mismatch" && f.Severity != wantSev {
				t.Errorf("%s: finding severity %s, want %s: %v", stage, f.Severity, wantSev, f.Evidence)
			}
		}
	}

	check("nothing uses either", true, true, model.SeverityInfo)
	exec(`CREATE TABLE pgbot_coll_col (v text COLLATE pgbot_coll_test)`)
	exec(`CREATE TABLE pgbot_coll_part (v text) PARTITION BY RANGE (v COLLATE "unicode")`)
	check("a column and a partition key", false, false, model.SeverityWarn)
	exec(`DROP TABLE pgbot_coll_part`)
	check("partition key dropped", false, true, model.SeverityWarn)
	exec(`CREATE TABLE pgbot_coll_unicode (v text COLLATE "unicode")`)
	check("a column on the pinned collation", false, false, model.SeverityWarn)
}
