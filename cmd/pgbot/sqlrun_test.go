package main

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSplitSQLStatements(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"select 1; select 2;", []string{"select 1", "select 2"}},
		{"select ';' as semi; select 2", []string{"select ';' as semi", "select 2"}},
		{"select 'it''s; fine'", []string{"select 'it''s; fine'"}},
		{`select E'a\'; b'`, []string{`select E'a\'; b'`}},
		{`select "we;ird" from t`, []string{`select "we;ird" from t`}},
		{"do $$ begin perform 1; end $$; select 3", []string{"do $$ begin perform 1; end $$", "select 3"}},
		{"select $fn$ x; y $fn$, $1", []string{"select $fn$ x; y $fn$, $1"}},
		{"select 1 -- trailing; comment\n; select 2", []string{"select 1 -- trailing; comment", "select 2"}},
		{"/* a; /* nested; */ still */ select 1", []string{"/* a; /* nested; */ still */ select 1"}},
		{"  ;; -- only a comment\n ;", nil},
	}
	for _, c := range cases {
		if got := splitSQLStatements(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("split(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRefuseStatement(t *testing.T) {
	allowed := []string{
		"select * from orders",
		"SELECT case when x then 1 end FROM t", // END inside CASE is not transaction control
		"with t as (select 1) select * from t",
		"explain select 1",
		"show work_mem",
		"set local work_mem = '64MB'",
		"select pg_size_pretty(pg_database_size(current_database()))",
		"(select 1) union (select 2)",
	}
	for _, s := range allowed {
		if r := refuseStatement(s); r != "" {
			t.Errorf("refused %q: %s", s, r)
		}
	}
	refused := []string{
		"commit", "END", "rollback", "begin read write", "start transaction", "savepoint a", "release a",
		"prepare transaction 'x'", "copy orders to '/tmp/x'", "COPY (select 1) TO PROGRAM 'id'",
		"lock table orders in access exclusive mode", "vacuum orders", "analyze orders", "checkpoint",
		"set session authorization postgres", "set role postgres",
		"select pg_terminate_backend(123)", "SELECT PG_CANCEL_BACKEND(1)", "select pg_reload_conf()",
		"select pg_stat_reset()", "select pg_stat_statements_reset()", "select * from dblink('x', 'delete from t')",
		"select lo_export(1, '/tmp/x')", "select pg_drop_replication_slot('s')",
		"/* sneaky */ commit", "  -- note\n  ROLLBACK",
	}
	for _, s := range refused {
		if refuseStatement(s) == "" {
			t.Errorf("allowed %q, want refused", s)
		}
	}
}

func TestRefusalMessageNamesTheStatement(t *testing.T) {
	if r := refuseStatement("commit"); !strings.Contains(r, "COMMIT") {
		t.Errorf("message %q should name COMMIT", r)
	}
}

func strp(s string) *string { return &s }

func TestRenderSQLRunText(t *testing.T) {
	out := sqlRunOutput{Results: []sqlRunResult{
		{Command: "SELECT 2", Columns: []sqlRunColumn{{Name: "id", Type: "integer"}, {Name: "note", Type: "text"}},
			Rows: [][]*string{{strp("1"), strp("a\nb")}, {strp("22"), nil}}, RowCount: 2},
		{Command: "SELECT 1", Columns: []sqlRunColumn{{Name: "x", Type: "text"}}, Rows: [][]*string{{strp("é")}}, RowCount: 1, Truncated: true},
		{Command: "SET"},
	}}
	var w, errw bytes.Buffer
	renderSQLRunText(&w, &errw, out)
	want := " id | note \n" +
		"----+------\n" +
		" 1  | a↵b  \n" +
		" 22 |      \n" +
		"(2 rows)\n\n" +
		" x \n" +
		"---\n" +
		" é \n" +
		"(first 1 row; more were not fetched)\n\n" +
		"SET\n\n"
	if w.String() != want {
		t.Errorf("aligned output:\n%s\nwant:\n%s", w.String(), want)
	}
	if errw.Len() != 0 {
		t.Errorf("no error expected on stderr, got %q", errw.String())
	}
}

func TestRenderSQLRunTextError(t *testing.T) {
	var w, errw bytes.Buffer
	renderSQLRunText(&w, &errw, sqlRunOutput{Error: &sqlRunError{Message: "cannot execute INSERT in a read-only transaction",
		SQLState: "25006", Hint: "read-only"}})
	want := "ERROR:  cannot execute INSERT in a read-only transaction (SQLSTATE 25006)\nHINT:  read-only\n"
	if errw.String() != want {
		t.Errorf("stderr = %q, want %q", errw.String(), want)
	}
	errw.Reset()
	renderSQLRunText(&w, &errw, sqlRunOutput{Error: &sqlRunError{Message: "refused", SQLState: "PGBOT"}})
	if strings.Contains(errw.String(), "SQLSTATE") {
		t.Errorf("a pgbot refusal has no server SQLSTATE: %q", errw.String())
	}
}

// The desktop app decodes this document (desktop/macos PSQL.swift). Renaming a
// key is a breaking change to sql_run_schema_version.
func TestSQLRunJSONContract(t *testing.T) {
	out := sqlRunOutput{SchemaVersion: sqlRunSchemaVersion, ReadOnly: true,
		Results: []sqlRunResult{{Statement: "select null", Command: "SELECT 1",
			Columns: []sqlRunColumn{{Name: "n", Type: "text"}}, Rows: [][]*string{{nil}}, RowCount: 1}},
		Error: &sqlRunError{StatementIndex: 1, Message: "m", SQLState: "42601", Position: 8}}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"sql_run_schema_version", "read_only", "results", "error", "elapsed_ms"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing top-level key %q in %s", k, b)
		}
	}
	r := got["results"].([]any)[0].(map[string]any)
	for _, k := range []string{"statement", "command", "columns", "rows", "row_count", "truncated", "elapsed_ms"} {
		if _, ok := r[k]; !ok {
			t.Errorf("missing result key %q", k)
		}
	}
	if cell := r["rows"].([]any)[0].([]any)[0]; cell != nil {
		t.Errorf("NULL must encode as JSON null, got %#v", cell)
	}
	e := got["error"].(map[string]any)
	for _, k := range []string{"statement_index", "message", "sqlstate", "position"} {
		if _, ok := e[k]; !ok {
			t.Errorf("missing error key %q", k)
		}
	}
	b, _ = json.Marshal(sqlRunOutput{Results: []sqlRunResult{}})
	if strings.Contains(string(b), `"error"`) {
		t.Errorf("error must be omitted on success: %s", b)
	}
}

// A refused statement is reported before any connection is attempted: the
// connection string below points at a port nothing listens on.
func TestExecuteSQLRunRefusesBeforeConnecting(t *testing.T) {
	f := sqlRunFlags{maxRows: 10, timeout: time.Second}
	out, err := executeSQLRun(context.Background(), "postgres://nobody@127.0.0.1:1/none?connect_timeout=1",
		"select 1; select pg_terminate_backend(1); select 2", f)
	if err != nil {
		t.Fatalf("a refusal is output, not an error: %v", err)
	}
	if out.Error == nil || out.Error.SQLState != "PGBOT" || out.Error.StatementIndex != 1 {
		t.Fatalf("want a PGBOT refusal of statement 1, got %+v", out.Error)
	}
	if len(out.Results) != 0 {
		t.Errorf("nothing may run when any statement is refused, got %d results", len(out.Results))
	}
	if _, err := executeSQLRun(context.Background(), "postgres://x@127.0.0.1:1/x", " -- nothing\n;", f); err == nil {
		t.Error("comment-only input should be a usage error")
	}
}
