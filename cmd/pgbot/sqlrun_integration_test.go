package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pgrundev/pggo"
)

// TestIntegration_sqlRun proves `pgbot sql run` against a real server: rows and
// type names come back, NULL stays distinct from "", --max-rows stops fetching,
// the statement timeout fires — and above all that a write, even by a
// superuser, never lands.
func TestIntegration_sqlRun(t *testing.T) {
	d := os.Getenv("PGBOT_TEST_SUPERUSER_DSN")
	if d == "" {
		t.Skip("set PGBOT_TEST_SUPERUSER_DSN to run the sql run integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pggo.Connect(ctx, d)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer admin.Close()
	if _, err := admin.SimpleQuery(ctx, `DROP TABLE IF EXISTS sqlrun_fixture;
		CREATE TABLE sqlrun_fixture (id int PRIMARY KEY, note text);
		INSERT INTO sqlrun_fixture SELECT g, CASE WHEN g % 2 = 0 THEN 'n' || g END FROM generate_series(1, 50) g`); err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), `DROP TABLE IF EXISTS sqlrun_fixture`) }()
	f := sqlRunFlags{maxRows: 500, timeout: 5 * time.Second}

	t.Run("rows, types and NULL", func(t *testing.T) {
		out, err := executeSQLRun(ctx, d, "select id, note from sqlrun_fixture where id <= 2 order by id; select 'x'::varchar(3) as v;", f)
		if err != nil || out.Error != nil {
			t.Fatalf("err=%v out.Error=%+v", err, out.Error)
		}
		if len(out.Results) != 2 {
			t.Fatalf("want 2 results, got %d", len(out.Results))
		}
		r := out.Results[0]
		if r.Columns[0].Type != "integer" || r.Columns[1].Type != "text" || out.Results[1].Columns[0].Type != "character varying" {
			t.Errorf("type names not resolved: %+v / %+v", r.Columns, out.Results[1].Columns)
		}
		if r.RowCount != 2 || r.Rows[0][1] != nil || r.Rows[1][1] == nil || *r.Rows[1][1] != "n2" {
			t.Errorf("rows = %v", r.Rows)
		}
		if r.Command != "SELECT 2" || r.Truncated {
			t.Errorf("command=%q truncated=%v", r.Command, r.Truncated)
		}
	})

	t.Run("max rows stops fetching", func(t *testing.T) {
		g := f
		g.maxRows = 10
		out, err := executeSQLRun(ctx, d, "select id from sqlrun_fixture order by id", g)
		if err != nil || out.Error != nil {
			t.Fatalf("err=%v out.Error=%+v", err, out.Error)
		}
		if r := out.Results[0]; r.RowCount != 10 || !r.Truncated {
			t.Errorf("row_count=%d truncated=%v, want 10/true", r.RowCount, r.Truncated)
		}
	})

	t.Run("writes and DDL are refused by the server and never land", func(t *testing.T) {
		for _, stmt := range []string{
			"delete from sqlrun_fixture",
			"update sqlrun_fixture set note = 'x'",
			"insert into sqlrun_fixture values (999, 'x')",
			"drop table sqlrun_fixture",
			"create table sqlrun_should_not_exist (a int)",
			"select 1; delete from sqlrun_fixture",
		} {
			out, err := executeSQLRun(ctx, d, stmt, f)
			if err != nil {
				t.Fatalf("%q: %v", stmt, err)
			}
			if out.Error == nil || out.Error.SQLState != "25006" {
				t.Errorf("%q: want SQLSTATE 25006, got %+v", stmt, out.Error)
			}
		}
		var n int
		var exists bool
		if err := admin.QueryRow(ctx, `SELECT count(*), to_regclass('sqlrun_should_not_exist') IS NOT NULL FROM sqlrun_fixture`).Scan(&n, &exists); err != nil {
			t.Fatal(err)
		}
		if n != 50 || exists {
			t.Fatalf("SAFETY VIOLATION: fixture has %d rows (want 50), stray table exists=%v", n, exists)
		}
	})

	t.Run("statement timeout and a syntax error position", func(t *testing.T) {
		g := f
		g.timeout = 300 * time.Millisecond
		out, err := executeSQLRun(ctx, d, "select pg_sleep(5)", g)
		if err != nil || out.Error == nil || out.Error.SQLState != "57014" {
			t.Errorf("want 57014, got err=%v %+v", err, out.Error)
		}
		out, err = executeSQLRun(ctx, d, "select 1; selec 2", f)
		if err != nil || out.Error == nil || out.Error.SQLState != "42601" || out.Error.StatementIndex != 1 || out.Error.Position != 1 {
			t.Errorf("want 42601 at statement 1 position 1, got err=%v %+v", err, out.Error)
		}
		if len(out.Results) != 1 {
			t.Errorf("the statement before the error still reports, got %d results", len(out.Results))
		}
	})
}
