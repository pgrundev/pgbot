package main

import (
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/pglog"
)

// --follow and -f are aliases of --live: same variable, any spelling works.
func TestFollowAliasesLive(t *testing.T) {
	for _, args := range [][]string{{"--live"}, {"--follow"}, {"-f"}} {
		cmd, f := logsCmdWithFlags()
		if err := cmd.Flags().Parse(args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !f.live {
			t.Errorf("%v must set live mode", args)
		}
	}
	cmd, f := logsCmdWithFlags()
	if err := cmd.Flags().Parse(nil); err != nil {
		t.Fatal(err)
	}
	if f.live {
		t.Error("live must default to off")
	}
}

func TestParseLevels(t *testing.T) {
	all, err := parseLevels("")
	if err != nil || !all[pglog.LevelQuery] || !all[pglog.LevelInfo] || !all[pglog.LevelWarn] || !all[pglog.LevelError] {
		t.Fatalf("empty filter must mean all levels: %v %v", all, err)
	}
	some, err := parseLevels("warn,error")
	if err != nil {
		t.Fatal(err)
	}
	if !some[pglog.LevelWarn] || !some[pglog.LevelError] || some[pglog.LevelQuery] || some[pglog.LevelInfo] {
		t.Errorf("filter wrong: %v", some)
	}
	if _, err := parseLevels("warn,bogus"); err == nil {
		t.Error("unknown level must be a usage error")
	}
	aud, err := parseLevels("audit")
	if err != nil || !aud[pglog.LevelAudit] || aud[pglog.LevelInfo] {
		t.Errorf("audit must be a filterable level: %v %v", aud, err)
	}
}

// pgbot must never show its own footprint: entries from its own backends (by
// PID) or labeled application_name=pgbot are dropped — otherwise, with
// log_min_duration_statement=0, the live tail is a feedback loop reading its
// own polling queries.
func TestIsSelfLogEntry(t *testing.T) {
	own := map[int]bool{72: true}
	if !isSelfLogEntry(pglog.Entry{PID: 72}, own) {
		t.Error("own backend PID must be filtered")
	}
	if !isSelfLogEntry(pglog.Entry{PID: 999, AppName: "pgbot"}, own) {
		t.Error("application_name=pgbot must be filtered")
	}
	if isSelfLogEntry(pglog.Entry{PID: 999, AppName: "psql"}, own) {
		t.Error("foreign entries must be kept")
	}
	// Historical lines from earlier pgbot runs have dead PIDs — only the
	// application_name can identify them, including the probe's nonce form.
	if !isSelfLogEntry(pglog.Entry{PID: 1, AppName: "pgbot_probe_42"}, own) {
		t.Error("probe nonce application_name must be filtered")
	}
	// Connection-authorized lines carry the app name only in the message text.
	if !isSelfLogEntry(pglog.Entry{PID: 1, Message: "connection authorized: user=ro database=d application_name=pgbot"}, own) {
		t.Error("connection-authorized pgbot lines must be filtered")
	}
	if isSelfLogEntry(pglog.Entry{PID: 1, Message: "connection authorized: user=ro database=d application_name=myapp"}, own) {
		t.Error("foreign connection lines must be kept")
	}
}

// The authenticated-phase line carries only the role identity; pgbot's own
// role authenticating is still pgbot's footprint.
func TestIsSelfConnUser(t *testing.T) {
	own := map[int]bool{}
	e := pglog.Entry{Message: `connection authenticated: identity="pgbot_ro" method=scram-sha-256 (pg_hba.conf:128)`}
	if !isSelfLogEntryForUser(e, own, "pgbot_ro") {
		t.Error("own role's authenticated line must be filtered")
	}
	if isSelfLogEntryForUser(e, own, "app") {
		t.Error("another role's authenticated line must be kept")
	}
	if isSelfLogEntryForUser(pglog.Entry{Message: "some pgbot_ro mention elsewhere"}, own, "pgbot_ro") {
		t.Error("only the authenticated-phase line matches, not any mention of the role")
	}
}

// --json output is a machine contract: literals scrubbed, one object per line.
func TestJSONLineIsScrubbed(t *testing.T) {
	e := pglog.Entry{
		Time:     time.Date(2026, 8, 31, 10, 0, 1, 0, time.UTC),
		Severity: "ERROR",
		Level:    pglog.LevelError,
		Message:  `duplicate key value violates unique constraint "users_email_key"`,
		Detail:   "Key (email)=(bob@example.com) already exists.\nINSERT INTO users (email) VALUES ('bob@example.com')",
	}
	line := jsonLogLine(e)
	if strings.Contains(line, "bob@example.com") {
		t.Errorf("PII leaked into --json: %s", line)
	}
	if !strings.Contains(line, `"level":"error"`) || !strings.Contains(line, "users_email_key") {
		t.Errorf("scrubbing must keep structure and identifiers: %s", line)
	}
	if strings.Contains(line, "\n") {
		t.Errorf("NDJSON line must be single-line: %q", line)
	}
}

// When the server isn't logging statements, the header must say so — a user
// tailing for queries otherwise stares at checkpoints wondering where the
// logs went.
func TestQueryLoggingNote(t *testing.T) {
	n := queryLoggingNote("-1", "none")
	if !strings.Contains(n, "log_min_duration_statement") {
		t.Errorf("off must produce a hint naming the setting: %q", n)
	}
	// Managed servers (allow_alter_system=off, RDS parameter groups) reject
	// ALTER SYSTEM — the note must carry the unblocked fallback.
	if !strings.Contains(n, "ALTER DATABASE") {
		t.Errorf("note must include the managed-server fallback: %q", n)
	}
	if n := queryLoggingNote("100ms", "none"); n != "" {
		t.Errorf("min-duration logging active — no note: %q", n)
	}
	if n := queryLoggingNote("-1", "all"); n != "" {
		t.Errorf("log_statement=all — no note: %q", n)
	}
	if n := queryLoggingNote("", ""); n != "" {
		t.Errorf("unknown settings (no permission) — stay quiet: %q", n)
	}
}
