package main

import (
	"strings"
	"testing"
)

func TestResolveSQLExplainInput(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("PGBOT_DATABASE_URL", "")
	t.Setenv("PGSERVICE", "")

	// One argument that looks like SQL is the query; connection comes from env.
	t.Setenv("DATABASE_URL", "postgres://env/db")
	conn, q, err := resolveSQLExplainInput([]string{"SELECT 1"}, strings.NewReader(""), true)
	if err != nil || conn != "postgres://env/db" || q != "SELECT 1" {
		t.Errorf("one arg: conn=%q q=%q err=%v", conn, q, err)
	}

	// Two arguments: connection string first, then the query.
	conn, q, err = resolveSQLExplainInput([]string{"postgres://arg/db", "select * from t"}, strings.NewReader(""), true)
	if err != nil || conn != "postgres://arg/db" || q != "select * from t" {
		t.Errorf("two args: conn=%q q=%q err=%v", conn, q, err)
	}

	// No argument and piped stdin: the query is read from stdin.
	conn, q, err = resolveSQLExplainInput(nil, strings.NewReader("SELECT 2\n"), false)
	if err != nil || conn != "postgres://env/db" || q != "SELECT 2" {
		t.Errorf("stdin: conn=%q q=%q err=%v", conn, q, err)
	}

	// A single connection-string argument with piped stdin: query from stdin.
	conn, q, err = resolveSQLExplainInput([]string{"postgres://arg/db"}, strings.NewReader("SELECT 3"), false)
	if err != nil || conn != "postgres://arg/db" || q != "SELECT 3" {
		t.Errorf("conn+stdin: conn=%q q=%q err=%v", conn, q, err)
	}

	// No argument and an interactive terminal: nothing to explain.
	if _, _, err = resolveSQLExplainInput(nil, strings.NewReader(""), true); err == nil {
		t.Error("no query on a TTY should be an error")
	}

	// No connection anywhere.
	t.Setenv("DATABASE_URL", "")
	if _, _, err = resolveSQLExplainInput([]string{"SELECT 1"}, strings.NewReader(""), true); err == nil || !strings.Contains(err.Error(), "connection") {
		t.Errorf("missing connection should error, got %v", err)
	}
}

func TestLooksLikeConnString(t *testing.T) {
	yes := []string{"postgres://u@h/db", "postgresql://h/db", "host=localhost dbname=x", "service=prod", "dbname=x"}
	no := []string{"SELECT 1", "select * from t where url = 'postgres://x'", "  SELECT host FROM t", ""}
	for _, s := range yes {
		if !looksLikeConnString(s) {
			t.Errorf("%q should look like a connection string", s)
		}
	}
	for _, s := range no {
		if looksLikeConnString(s) {
			t.Errorf("%q should not look like a connection string", s)
		}
	}
}
