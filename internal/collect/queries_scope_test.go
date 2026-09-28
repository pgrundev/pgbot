package collect

import (
	_ "embed"
	"strings"
	"testing"
)

// Regression guard: pg_stat_statements is cluster-wide, and a role holding
// pg_read_all_stats (directly or through pg_monitor) can read EVERY
// database's rows from it. Without an explicit scope, pgbot connects to
// database A and quietly reports database B's statements as A's top queries —
// cross-database leakage in every snapshot. queries.sql must therefore pin
// each pg_stat_statements read to the connected database via
// dbid = (SELECT oid FROM pg_database WHERE datname = current_database()).
// Like the read-only tripwire next door, this is a cheap, always-runs static
// check (no database, no env vars, no build tags) that fails the moment the
// scope filter is dropped or edited past recognition — long before a
// multi-database cluster hides the leak again.

//go:embed sql/queries.sql
var queriesScopeSQL string

// queriesScopeFilter is the exact predicate, as written in sql/queries.sql,
// that pins a pg_stat_statements read to the connected database.
const queriesScopeFilter = "dbid = (SELECT oid FROM pg_database WHERE datname = current_database())"

func TestQueriesScope(t *testing.T) {
	if strings.TrimSpace(queriesScopeSQL) == "" {
		t.Fatal("no embedded SQL read from sql/queries.sql — the guard is not scanning anything")
	}
	found := false
	for i, stmt := range strings.Split(stripSQLComments(queriesScopeSQL), ";") {
		if !strings.Contains(stmt, "pg_stat_statements") {
			continue // statement does not touch the view
		}
		found = true
		if !strings.Contains(stmt, queriesScopeFilter) {
			t.Errorf("statement %d reads pg_stat_statements without the connected-database scope %q — a pg_read_all_stats role would leak every other database's statements into this one's top queries", i+1, queriesScopeFilter)
		}
	}
	if !found {
		t.Fatal("no statement in sql/queries.sql references pg_stat_statements — the scope guard matched nothing and would pass vacuously")
	}
}
