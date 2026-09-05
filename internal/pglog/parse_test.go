package pglog

import (
	"strings"
	"testing"
)

// --- stderr format (logging_collector stderr, default PGDG prefix '%m [%p] ') ---

const stderrFixture = `2026-08-31 10:00:01.123 UTC [881] LOG:  checkpoint starting: time
2026-08-31 10:00:02.500 UTC [902] LOG:  duration: 812.412 ms  statement: SELECT * FROM logs WHERE created_at > '2026-01-01'
2026-08-31 10:00:03.001 UTC [903] ERROR:  duplicate key value violates unique constraint "users_email_key"
2026-08-31 10:00:03.001 UTC [903] DETAIL:  Key (email)=(bob@example.com) already exists.
2026-08-31 10:00:03.001 UTC [903] STATEMENT:  INSERT INTO users (email) VALUES ('bob@example.com')
2026-08-31 10:00:04.777 UTC [904] WARNING:  temporary file: path "base/pgsql_tmp/pgsql_tmp904.0", size 24576
2026-08-31 10:00:05.100 UTC [905] LOG:  statement: BEGIN
2026-08-31 10:00:06.000 UTC [881] LOG:  automatic vacuum of table "prod.public.orders": index scans: 1
	pages: 0 removed, 620 remain, 620 scanned (100.00% of total)
	tuples: 118 removed, 634 remain, 0 are dead but not yet removable
`

func TestParseStderrEntriesAndLevels(t *testing.T) {
	entries := parseAll(t, FormatStderr, stderrFixture)
	if len(entries) != 6 {
		t.Fatalf("want 6 entries, got %d: %+v", len(entries), entries)
	}

	e := entries[0]
	if e.Level != LevelInfo || !strings.Contains(e.Message, "checkpoint starting") {
		t.Errorf("checkpoint entry wrong: %+v", e)
	}
	if got := e.Time.Format("15:04:05.000"); got != "10:00:01.123" {
		t.Errorf("timestamp = %s", got)
	}
	if e.PID != 881 {
		t.Errorf("pid = %d", e.PID)
	}

	if e := entries[1]; e.Level != LevelQuery || e.Severity != "LOG" {
		t.Errorf("duration line must be level query: %+v", e)
	}
	if e := entries[2]; e.Level != LevelError ||
		!strings.Contains(e.Detail, "already exists") ||
		!strings.Contains(e.Detail, "INSERT INTO users") {
		t.Errorf("error must absorb DETAIL and STATEMENT continuations: %+v", e)
	}
	if e := entries[3]; e.Level != LevelWarn {
		t.Errorf("temp file must be warn: %+v", e)
	}
	if e := entries[4]; e.Level != LevelQuery {
		t.Errorf("statement: line must be level query: %+v", e)
	}
	// Tab-continuation lines belong to the autovacuum entry, not new entries.
	if e := entries[5]; !strings.Contains(e.Message, "automatic vacuum") ||
		!strings.Contains(e.Detail, "620 scanned") {
		t.Errorf("multiline vacuum entry wrong: %+v", e)
	}
}

// An RDS-style prefix (%t:%r:%u@%d:[%p]:) must still yield entries with user/db.
const stderrRDSFixture = `2026-08-31 10:00:01 UTC:10.0.4.12(51422):app@prod:[904]:ERROR:  relation "old_sessions" does not exist
2026-08-31 10:00:02 UTC:10.0.4.12(51422):app@prod:[904]:LOG:  duration: 3.210 ms  statement: SELECT count(*) FROM orders
`

func TestParseStderrRDSPrefix(t *testing.T) {
	entries := parseAll(t, FormatStderr, stderrRDSFixture)
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	if e := entries[0]; e.Level != LevelError || e.User != "app" || e.Database != "prod" || e.PID != 904 {
		t.Errorf("rds error entry wrong: %+v", e)
	}
	if e := entries[1]; e.Level != LevelQuery {
		t.Errorf("rds duration entry wrong: %+v", e)
	}
}

// --- csvlog: quoted multiline message and embedded commas must survive ---

const csvFixture = `2026-08-31 10:00:01.123 UTC,"app","prod",902,"10.0.4.12:51422",68b3f001.386,1,"SELECT",2026-08-31 09:59:59 UTC,3/42,0,LOG,00000,"duration: 812.412 ms  statement: SELECT * FROM logs WHERE created_at > '2026-01-01'",,,,,,,,,"psql","client backend",,0
2026-08-31 10:00:03.001 UTC,"app","prod",903,"10.0.4.12:51423",68b3f002.387,1,"INSERT",2026-08-31 09:59:59 UTC,4/13,777,ERROR,23505,"duplicate key value violates unique constraint ""users_email_key""","Key (email)=(bob@example.com) already exists.",,,,,"INSERT INTO users (email)
VALUES ('bob@example.com')",,"app","client backend",,0
`

func TestParseCSV(t *testing.T) {
	entries := parseAll(t, FormatCSV, csvFixture)
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(entries), entries)
	}
	if e := entries[0]; e.Level != LevelQuery || e.User != "app" || e.Database != "prod" || e.PID != 902 || e.AppName != "psql" {
		t.Errorf("csv query entry wrong: %+v", e)
	}
	e := entries[1]
	if e.Level != LevelError || e.Severity != "ERROR" {
		t.Errorf("csv error entry wrong: %+v", e)
	}
	if !strings.Contains(e.Message, `"users_email_key"`) {
		t.Errorf("escaped quotes must survive: %q", e.Message)
	}
	if !strings.Contains(e.Detail, "already exists") || !strings.Contains(e.Detail, "VALUES") {
		t.Errorf("detail must include detail column and multiline query: %q", e.Detail)
	}
}

// --- jsonlog (PG15+) ---

const jsonFixture = `{"timestamp":"2026-08-31 10:00:01.123 UTC","user":"app","dbname":"prod","pid":902,"error_severity":"LOG","message":"duration: 812.412 ms  statement: SELECT * FROM logs WHERE created_at > '2026-01-01'","application_name":"psql","backend_type":"client backend","query_id":0}
{"timestamp":"2026-08-31 10:00:03.001 UTC","user":"app","dbname":"prod","pid":903,"error_severity":"ERROR","state_code":"23505","message":"duplicate key value violates unique constraint \"users_email_key\"","detail":"Key (email)=(bob@example.com) already exists.","statement":"INSERT INTO users (email) VALUES ('bob@example.com')","backend_type":"client backend"}
{"timestamp":"2026-08-31 10:00:05.000 UTC","pid":881,"error_severity":"WARNING","message":"temporary file: path \"x\", size 24576"}
`

func TestParseJSON(t *testing.T) {
	entries := parseAll(t, FormatJSON, jsonFixture)
	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(entries))
	}
	if e := entries[0]; e.Level != LevelQuery || e.User != "app" || e.PID != 902 || e.AppName != "psql" {
		t.Errorf("json query entry wrong: %+v", e)
	}
	if e := entries[1]; e.Level != LevelError || !strings.Contains(e.Detail, "INSERT INTO users") {
		t.Errorf("json error entry must carry detail+statement: %+v", e)
	}
	if e := entries[2]; e.Level != LevelWarn {
		t.Errorf("json warn entry wrong: %+v", e)
	}
}

func TestFormatForFile(t *testing.T) {
	cases := map[string]Format{
		"postgresql-2026-08-31_100000.json": FormatJSON,
		"postgresql-2026-08-31_100000.csv":  FormatCSV,
		"postgresql-2026-08-31_100000.log":  FormatStderr,
		"postgresql.log":                    FormatStderr,
	}
	for name, want := range cases {
		if got := FormatForFile(name); got != want {
			t.Errorf("FormatForFile(%s) = %v, want %v", name, got, want)
		}
	}
}

// Severity → pgbot level, independent of source format.
func TestLevelFor(t *testing.T) {
	cases := []struct {
		sev, msg string
		want     Level
	}{
		{"ERROR", "x", LevelError},
		{"FATAL", "x", LevelError},
		{"PANIC", "x", LevelError},
		{"WARNING", "x", LevelWarn},
		{"LOG", "checkpoint complete", LevelInfo},
		{"LOG", "duration: 3.2 ms  statement: SELECT 1", LevelQuery},
		{"LOG", "statement: COMMIT", LevelQuery},
		{"LOG", "execute stmt_1: SELECT 1", LevelQuery},
		{"NOTICE", "x", LevelInfo},
		{"DEBUG1", "x", LevelInfo},
		// pgAudit writes its trail as LOG lines prefixed AUDIT: — they get
		// their own level so `pgbot logs --level audit` reads the audit trail.
		{"LOG", "AUDIT: SESSION,1,1,READ,SELECT,,,SELECT * FROM accounts,<none>", LevelAudit},
	}
	for _, c := range cases {
		if got := levelFor(c.sev, c.msg); got != c.want {
			t.Errorf("levelFor(%s, %q) = %v, want %v", c.sev, c.msg, got, c.want)
		}
	}
}

func parseAll(t *testing.T, f Format, data string) []Entry {
	t.Helper()
	p := NewParser(f)
	entries := p.Parse([]byte(data))
	entries = append(entries, p.Flush()...)
	return entries
}
