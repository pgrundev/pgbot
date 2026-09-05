package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/pglog"
	"github.com/pgrundev/pgbot/internal/render"
	"github.com/spf13/cobra"
)

type logsFlags struct {
	last    int
	live    bool
	level   string
	jsonOut bool
	noColor bool
	timeout time.Duration
}

// newLogsCmd — `pgbot logs`. Tail the server's own logfile over SQL: the last
// N entries, or --live streaming. Experimental.
func newLogsCmd() *cobra.Command {
	cmd, _ := logsCmdWithFlags()
	return cmd
}

// logsCmdWithFlags builds the command and exposes its flag struct, so tests
// can parse arguments and observe what they resolved to.
func logsCmdWithFlags() (*cobra.Command, *logsFlags) {
	f := &logsFlags{}
	cmd := &cobra.Command{
		Use:   "logs <connection-string>",
		Short: "Read the server log — last N entries, or follow live (experimental)",
		Long: `Reads the PostgreSQL server log over SQL (pg_current_logfile + pg_read_binary_file)
and prints typed entries: query, info, warn, error. No agent, no sidecar.

Beyond pg_monitor this needs one extra grant, printed when missing. The human
output shows log lines verbatim; --json scrubs literals (the machine contract).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogs(cmd, args, *f)
		},
	}
	fl := cmd.Flags()
	fl.IntVar(&f.last, "last", 100, "how many of the newest entries to print")
	fl.BoolVar(&f.live, "live", false, "keep following the log after printing the newest entries")
	// --follow / -f is the tail -f muscle-memory spelling: a true alias, bound
	// to the same variable as --live.
	fl.BoolVarP(&f.live, "follow", "f", false, "alias for --live")
	fl.StringVar(&f.level, "level", "", "only these levels, comma-separated: query,info,warn,error,audit (default all)")
	fl.BoolVar(&f.jsonOut, "json", false, "one JSON object per entry (literals scrubbed)")
	fl.BoolVar(&f.noColor, "no-color", false, "disable ANSI color")
	fl.DurationVar(&f.timeout, "timeout", 30*time.Second, "wall-clock budget (ignored with --live)")
	return cmd, f
}

func runLogs(cmd *cobra.Command, args []string, f logsFlags) error {
	connString := firstNonEmpty(argAt(args, 0), os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"))
	if connString == "" {
		return fmt.Errorf("no connection string (pass one or set $DATABASE_URL)")
	}
	if f.last < 1 {
		return usageErrf("--last must be at least 1")
	}
	levels, err := parseLevels(f.level)
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	if !f.live {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.timeout)
		defer cancel()
	}

	target, err := conn.Connect(ctx, connString)
	if err != nil {
		return err
	}
	defer target.Close()

	// pgbot never reports its own footprint: the connect probe and every poll
	// this command runs write log lines of their own (log_min_duration_statement=0
	// logs everything), and an unfiltered live tail would read its own reads.
	// The PID set is consulted per entry, not snapshotted: pool backends
	// register on first use, and a long --live recycles connections (5m
	// lifetime), minting new PIDs the whole while.
	connUser := ""
	if cfg, cerr := pgconn.ParseConfig(connString); cerr == nil {
		connUser = cfg.User
	}
	keep := func(e pglog.Entry) bool {
		own := map[int]bool{}
		for _, pid := range target.SelfPIDs() {
			own[int(pid)] = true
		}
		return !isSelfLogEntryForUser(e, own, connUser) && levels[e.Level]
	}

	src, err := pglog.NewSQLSource(ctx, target.Pool)
	if err != nil {
		return logsErr(err, connString)
	}
	entries, pos, err := pglog.LastN(ctx, src, f.last, keep)
	if err != nil {
		return logsErr(err, connString)
	}

	st := render.NewStyler(useColor(f.noColor))
	if !f.jsonOut {
		mode := fmt.Sprintf("last %d", f.last)
		if f.live {
			mode += " · live (Ctrl+C to stop)"
		}
		fmt.Printf("%s · %s · %s\n", st.Head("pgbot logs"), st.Dim(pos.Name), st.Dim(mode))
		// Best-effort: unreadable settings just skip the note.
		var minDur, logStmt string
		_ = target.Pool.QueryRow(ctx,
			"SELECT current_setting('log_min_duration_statement'), current_setting('log_statement')").
			Scan(&minDur, &logStmt)
		if note := queryLoggingNote(minDur, logStmt); note != "" {
			fmt.Println(st.Dim(note))
		}
		fmt.Println()
	}
	emit := func(e pglog.Entry) error {
		if !keep(e) {
			return nil
		}
		if f.jsonOut {
			fmt.Println(jsonLogLine(e))
			return nil
		}
		printLogEntry(st, e)
		return nil
	}
	for _, e := range entries {
		_ = emit(e)
	}
	if !f.live {
		return nil
	}
	err = pglog.Follow(ctx, src, pos, time.Second, emit)
	if errors.Is(err, context.Canceled) {
		return nil // Ctrl+C is how a live tail ends
	}
	return err
}

// logsErr turns the two expected failures into their fixes: no collector, and
// the one missing GRANT.
func logsErr(err error, connString string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42501" { // insufficient_privilege
		user := "your_pgbot_role"
		if cfg, cerr := pgconn.ParseConfig(connString); cerr == nil && cfg.User != "" {
			user = cfg.User
		}
		return fmt.Errorf("reading the server log needs one grant beyond pg_monitor — run as an admin:\n\n  %s\n\n(%s)",
			pglog.ReadGrantSQL(user), pgErr.Message)
	}
	return err
}

// isSelfLogEntry reports whether a log entry was produced by one of pgbot's
// own backends. Live backends match by PID; historical lines from earlier runs
// have dead PIDs, so the application_name identifies them — the pinned "pgbot",
// the probe's "pgbot_probe_<nonce>", or, for connection-phase lines where only
// the message text carries it, "application_name=pgbot…" in the message.
// (Unlike pg_stat_activity's PID-only self-exclusion, a name match here is
// spoofable — but this only hides lines from pgbot's view; the server log
// itself is untouched.)
func isSelfLogEntry(e pglog.Entry, ownPIDs map[int]bool) bool {
	return strings.HasPrefix(e.AppName, "pgbot") ||
		(e.PID != 0 && ownPIDs[e.PID]) ||
		strings.Contains(e.Message, "application_name=pgbot")
}

// isSelfLogEntryForUser additionally drops the authenticated-phase line for
// pgbot's own role — the one connection line that names neither PID context
// nor application_name, only the identity.
func isSelfLogEntryForUser(e pglog.Entry, ownPIDs map[int]bool, user string) bool {
	if isSelfLogEntry(e, ownPIDs) {
		return true
	}
	return user != "" &&
		strings.HasPrefix(e.Message, "connection authenticated: ") &&
		strings.Contains(e.Message, `identity="`+user+`"`)
}

// queryLoggingNote explains an all-noise stream: with log_min_duration_statement
// = -1 and log_statement = none the server writes no query lines at all, and a
// user tailing for queries stares at checkpoints wondering where the logs went.
// Empty settings (couldn't read them) stay quiet rather than guess.
func queryLoggingNote(minDuration, logStatement string) string {
	if minDuration != "-1" || logStatement != "none" {
		return ""
	}
	return "note: this server logs no query lines (log_min_duration_statement=-1, log_statement=none) —\n" +
		"      only errors, checkpoints, and connections will appear. To log slow queries:\n" +
		"      ALTER SYSTEM SET log_min_duration_statement = '100ms'; SELECT pg_reload_conf();\n" +
		"      (managed servers may block ALTER SYSTEM; then, applying to new sessions:\n" +
		"       ALTER DATABASE <yourdb> SET log_min_duration_statement = '100ms';)"
}

// parseLevels turns "warn,error" into a set; empty means every level.
func parseLevels(s string) (map[pglog.Level]bool, error) {
	all := map[pglog.Level]bool{
		pglog.LevelQuery: true, pglog.LevelInfo: true, pglog.LevelWarn: true,
		pglog.LevelError: true, pglog.LevelAudit: true,
	}
	if strings.TrimSpace(s) == "" {
		return all, nil
	}
	set := map[pglog.Level]bool{}
	for _, part := range strings.Split(s, ",") {
		l := pglog.Level(strings.TrimSpace(strings.ToLower(part)))
		if !all[l] {
			return nil, usageErrf("unknown --level %q (valid: query, info, warn, error, audit)", part)
		}
		set[l] = true
	}
	return set, nil
}

// jsonLogLine renders one entry as a single scrubbed NDJSON line — the
// machine contract. Message and detail pass through the same literal scrubber
// as every other query text pgbot emits.
func jsonLogLine(e pglog.Entry) string {
	e.Message = conn.ScrubQueryText(e.Message)
	e.Detail = conn.ScrubQueryText(e.Detail)
	e.Detail = strings.ReplaceAll(e.Detail, "\n", " ")
	b, err := json.Marshal(e)
	if err != nil {
		return `{"level":"error","message":"pgbot: failed to encode log entry"}`
	}
	return string(b)
}

func printLogEntry(st render.Styler, e pglog.Entry) {
	ts := "            "
	if !e.Time.IsZero() {
		ts = e.Time.Format("15:04:05.000")
	}
	level := string(e.Level)
	switch e.Level {
	case pglog.LevelError:
		level = st.Crit(level)
	case pglog.LevelWarn:
		level = st.Warn(level)
	case pglog.LevelQuery:
		level = st.Head(level)
	case pglog.LevelAudit:
		level = st.AI(level)
	default:
		level = st.Dim(level)
	}
	fmt.Printf("%s  %-5s  %s\n", st.Dim(ts), level, e.Message)
	if e.Detail != "" {
		for _, line := range strings.Split(e.Detail, "\n") {
			fmt.Printf("              %s\n", st.Dim("↳ "+line))
		}
	}
}
