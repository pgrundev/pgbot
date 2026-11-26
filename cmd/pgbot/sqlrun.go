package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pggo"
	"github.com/spf13/cobra"
)

// `pgbot sql run` executes the user's own SQL — the psql-style console of the
// desktop app — without giving up pgbot's read-only guarantee:
//
//   - every statement runs inside ONE `BEGIN READ ONLY` transaction that is
//     always rolled back, so the server itself refuses writes and DDL;
//   - statements that could end or escape that transaction (COMMIT, BEGIN,
//     SAVEPOINT, …), reach outside the database (COPY, dblink, large-object
//     file I/O) or act on other sessions or the server (pg_terminate_backend,
//     pg_reload_conf, stats resets, replication-slot changes, …) are refused
//     before anything is sent;
//   - each statement goes over the extended protocol, which carries exactly one
//     statement, so "SELECT 1; COMMIT" cannot be smuggled into one call;
//   - a statement timeout and a lock timeout bound every statement, and result
//     sets are cut off server-side at --max-rows.
func newSQLRunCmd() *cobra.Command {
	var f sqlRunFlags
	cmd := &cobra.Command{
		Use:   `run [connection-string] "<SQL>"`,
		Short: "Run your own SQL read-only (psql-style), as a table or JSON",
		Long: "Executes one or more statements inside a single READ ONLY transaction that is\n" +
			"always rolled back. Writes and DDL are refused by the server; transaction\n" +
			"control, COPY, dblink, large-object file I/O and functions that act on other\n" +
			"sessions or the server (pg_terminate_backend, pg_reload_conf, stats resets,\n" +
			"replication-slot changes, …) are refused by pgbot before anything is sent.\n\n" +
			"The SQL is the last argument, or stdin when piped. The connection is the first\n" +
			"argument, else $DATABASE_URL / $PGBOT_DATABASE_URL / $PGSERVICE.",
		Example: "  pgbot sql run \"SELECT now()\"\n" +
			"  echo 'SELECT relname, n_live_tup FROM pg_stat_user_tables ORDER BY 2 DESC LIMIT 5;' | pgbot sql run --json",
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error { return runSQLRun(cmd, args, f) },
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.jsonOut, "json", false, "emit results as JSON (sql_run_schema_version 1.0.0)")
	fl.IntVar(&f.maxRows, "max-rows", 500, "rows returned per statement; the rest are never fetched")
	fl.DurationVar(&f.timeout, "statement-timeout", 30*time.Second, "per-statement timeout")
	return cmd
}

type sqlRunFlags struct {
	jsonOut bool
	maxRows int
	timeout time.Duration
}

const sqlRunSchemaVersion = "1.0.0"

type sqlRunColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type sqlRunResult struct {
	Statement string         `json:"statement"`
	Command   string         `json:"command"`
	Columns   []sqlRunColumn `json:"columns"`
	Rows      [][]*string    `json:"rows"`
	RowCount  int            `json:"row_count"`
	Truncated bool           `json:"truncated"`
	ElapsedMS float64        `json:"elapsed_ms"`
}

type sqlRunError struct {
	StatementIndex int    `json:"statement_index"` // 0-based; -1 when not tied to a statement
	Statement      string `json:"statement,omitempty"`
	Message        string `json:"message"`
	SQLState       string `json:"sqlstate,omitempty"` // "PGBOT" for a pgbot refusal
	Detail         string `json:"detail,omitempty"`
	Hint           string `json:"hint,omitempty"`
	Position       int    `json:"position,omitempty"` // 1-based character offset into the statement
}

type sqlRunOutput struct {
	SchemaVersion string         `json:"sql_run_schema_version"`
	ReadOnly      bool           `json:"read_only"`
	Results       []sqlRunResult `json:"results"`
	Error         *sqlRunError   `json:"error,omitempty"`
	ElapsedMS     float64        `json:"elapsed_ms"`
}

func runSQLRun(cmd *cobra.Command, args []string, f sqlRunFlags) error {
	if f.maxRows < 1 || f.maxRows > 100000 {
		return usageErrf("--max-rows must be between 1 and 100000, got %d", f.maxRows)
	}
	connString, input, err := resolveSQLExplainInput(args, os.Stdin, isInteractive())
	if err != nil {
		return err
	}
	out, err := executeSQLRun(cmd.Context(), connString, input, f)
	if err != nil {
		return err
	}
	return emitSQLRun(cmd.OutOrStdout(), f.jsonOut, out)
}

// executeSQLRun does the work of `pgbot sql run` and returns the output
// document. A refusal or a statement error is reported in out.Error (with
// err nil); err is reserved for usage, connection and session failures.
// Everything is rolled back and the connection closed before it returns.
func executeSQLRun(ctx context.Context, connString, input string, f sqlRunFlags) (sqlRunOutput, error) {
	out := sqlRunOutput{SchemaVersion: sqlRunSchemaVersion, ReadOnly: true, Results: []sqlRunResult{}}
	started := time.Now()
	finish := func() (sqlRunOutput, error) {
		out.ElapsedMS = float64(time.Since(started).Microseconds()) / 1000
		return out, nil
	}

	stmts := splitSQLStatements(input)
	if len(stmts) == 0 {
		return out, usageErrf("no SQL statement given")
	}
	for i, s := range stmts {
		if reason := refuseStatement(s); reason != "" {
			out.Error = &sqlRunError{StatementIndex: i, Statement: s, SQLState: "PGBOT", Message: reason,
				Hint: "pgbot runs SQL read-only: queries and catalog lookups work; changes, transaction control and server actions don't."}
			return finish()
		}
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(len(stmts))*f.timeout+15*time.Second)
	defer cancel()
	target, err := conn.Connect(ctx, connString)
	if err != nil {
		return out, fmt.Errorf("connect: %s", conn.RedactConnString(err.Error()))
	}
	defer target.Close()

	tx, err := target.Pool.BeginTx(ctx, pggo.TxOptions{ReadOnly: true})
	if err != nil {
		return out, fmt.Errorf("begin read-only transaction: %w", err)
	}
	// Always roll back: nothing here may persist, not even a SET.
	defer func() { _ = tx.Rollback(context.Background()) }()
	for _, s := range []string{
		fmt.Sprintf("SET LOCAL statement_timeout = %d", f.timeout.Milliseconds()),
		"SET LOCAL lock_timeout = 5000",
		"SET LOCAL idle_in_transaction_session_timeout = 60000",
	} {
		if _, err := tx.Exec(ctx, s); err != nil {
			return out, fmt.Errorf("prepare session: %w", err)
		}
	}

	typeOIDs := map[uint32]bool{}
	for i, s := range stmts {
		res, err := runOneStatement(ctx, tx, s, f.maxRows)
		if err != nil {
			out.Error = toSQLRunError(i, s, err)
			break
		}
		for _, c := range res.oids {
			typeOIDs[c] = true
		}
		out.Results = append(out.Results, res.result)
	}
	// Type names come from the server's own catalog (format_type), resolved
	// once for every column type seen. A failed statement aborts the
	// transaction, so this is best-effort; numeric OIDs are the fallback.
	names := map[uint32]string{}
	if out.Error == nil && len(typeOIDs) > 0 {
		names = typeNames(ctx, tx, typeOIDs)
	}
	for ri := range out.Results {
		for ci := range out.Results[ri].Columns {
			if oid, err := strconv.ParseUint(out.Results[ri].Columns[ci].Type, 10, 32); err == nil {
				if n, ok := names[uint32(oid)]; ok {
					out.Results[ri].Columns[ci].Type = n
				}
			}
		}
	}
	return finish()
}

type statementRun struct {
	result sqlRunResult
	oids   []uint32
}

func runOneStatement(ctx context.Context, tx *pggo.Tx, stmt string, maxRows int) (statementRun, error) {
	t0 := time.Now()
	rows, err := tx.Query(ctx, stmt, pggo.MaxRows(maxRows))
	if err != nil {
		return statementRun{}, err
	}
	defer rows.Close()
	var run statementRun
	cols := rows.Columns()
	run.result.Statement = stmt
	run.result.Columns = make([]sqlRunColumn, len(cols))
	for i, c := range cols {
		run.result.Columns[i] = sqlRunColumn{Name: c.Name, Type: strconv.FormatUint(uint64(c.TypeOID), 10)}
		run.oids = append(run.oids, c.TypeOID)
	}
	run.result.Rows = [][]*string{}
	for rows.Next() {
		raw := rows.RawValues()
		row := make([]*string, len(raw))
		for i, v := range raw {
			if v != nil {
				s := string(v)
				row[i] = &s
			}
		}
		run.result.Rows = append(run.result.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return statementRun{}, err
	}
	run.result.RowCount = len(run.result.Rows)
	run.result.Truncated = rows.Suspended()
	run.result.Command = string(rows.CommandTag())
	if run.result.Command == "" && run.result.Truncated {
		run.result.Command = fmt.Sprintf("SELECT %d", run.result.RowCount) // a suspended portal sends no tag
	}
	run.result.ElapsedMS = float64(time.Since(t0).Microseconds()) / 1000
	return run, nil
}

func typeNames(ctx context.Context, tx *pggo.Tx, oids map[uint32]bool) map[uint32]string {
	list := make([]string, 0, len(oids))
	for o := range oids {
		list = append(list, strconv.FormatUint(uint64(o), 10))
	}
	sort.Strings(list)
	out := map[uint32]string{}
	rows, err := tx.Query(ctx, "SELECT oid::int8, format_type(oid, NULL) FROM pg_type WHERE oid IN ("+strings.Join(list, ",")+")")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var oid int64
		var name string
		if rows.Scan(&oid, &name) == nil {
			out[uint32(oid)] = name
		}
	}
	return out
}

func toSQLRunError(i int, stmt string, err error) *sqlRunError {
	var pe *pggo.PgError
	if errors.As(err, &pe) {
		e := &sqlRunError{StatementIndex: i, Statement: stmt, Message: pe.Message, SQLState: pe.Code,
			Detail: pe.Detail, Hint: pe.Hint, Position: pe.Position}
		switch pe.Code {
		case "25006": // read_only_sql_transaction
			e.Hint = "pgbot runs SQL read-only. Copy the statement and run it yourself if you mean to change the database."
		case "57014": // query_canceled (statement_timeout)
			e.Hint = "The statement hit pgbot's statement timeout. Narrow it (LIMIT, WHERE) or raise --statement-timeout."
		}
		return e
	}
	return &sqlRunError{StatementIndex: i, Statement: stmt, Message: conn.RedactConnString(err.Error())}
}

func emitSQLRun(w io.Writer, jsonOut bool, out sqlRunOutput) error {
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return err
		}
	} else {
		renderSQLRunText(w, os.Stderr, out)
	}
	if out.Error != nil {
		os.Exit(exitFailure) // the output above already explains it
	}
	return nil
}

// renderSQLRunText prints psql's "aligned" format: results to w, an error to errw.
func renderSQLRunText(w, errw io.Writer, out sqlRunOutput) {
	for _, r := range out.Results {
		if len(r.Columns) == 0 {
			fmt.Fprintln(w, r.Command)
			fmt.Fprintln(w)
			continue
		}
		widths := make([]int, len(r.Columns))
		for i, c := range r.Columns {
			widths[i] = utf8.RuneCountInString(c.Name)
		}
		cell := func(v *string) string {
			if v == nil {
				return ""
			}
			return strings.ReplaceAll(*v, "\n", "↵")
		}
		for _, row := range r.Rows {
			for i, v := range row {
				if n := utf8.RuneCountInString(cell(v)); n > widths[i] {
					widths[i] = n
				}
			}
		}
		pad := func(s string, n int) string { return s + strings.Repeat(" ", n-utf8.RuneCountInString(s)) }
		var head, rule []string
		for i, c := range r.Columns {
			head = append(head, " "+pad(c.Name, widths[i])+" ")
			rule = append(rule, strings.Repeat("-", widths[i]+2))
		}
		fmt.Fprintln(w, strings.Join(head, "|"))
		fmt.Fprintln(w, strings.Join(rule, "+"))
		for _, row := range r.Rows {
			var cells []string
			for i, v := range row {
				cells = append(cells, " "+pad(cell(v), widths[i])+" ")
			}
			fmt.Fprintln(w, strings.Join(cells, "|"))
		}
		suffix := "s"
		if r.RowCount == 1 {
			suffix = ""
		}
		if r.Truncated {
			fmt.Fprintf(w, "(first %d row%s; more were not fetched)\n\n", r.RowCount, suffix)
		} else {
			fmt.Fprintf(w, "(%d row%s)\n\n", r.RowCount, suffix)
		}
	}
	if e := out.Error; e != nil {
		fmt.Fprintf(errw, "ERROR:  %s", e.Message)
		if e.SQLState != "" && e.SQLState != "PGBOT" {
			fmt.Fprintf(errw, " (SQLSTATE %s)", e.SQLState)
		}
		fmt.Fprintln(errw)
		if e.Detail != "" {
			fmt.Fprintln(errw, "DETAIL:  "+e.Detail)
		}
		if e.Hint != "" {
			fmt.Fprintln(errw, "HINT:  "+e.Hint)
		}
	}
}

// splitSQLStatements splits on top-level semicolons, respecting quoted strings
// ('…' with doubled-quote escapes, E'…' with backslash escapes), quoted identifiers,
// dollar-quoted bodies ($$…$$, $tag$…$tag$), and -- and nested /* */ comments.
// Empty and comment-only statements are dropped.
func splitSQLStatements(sql string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		s := strings.TrimSpace(b.String())
		if s != "" && strings.TrimSpace(stripSQLComments(s)) != "" {
			out = append(out, s)
		}
		b.Reset()
	}
	rs := []rune(sql)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '-' && i+1 < len(rs) && rs[i+1] == '-':
			for i < len(rs) && rs[i] != '\n' {
				b.WriteRune(rs[i])
				i++
			}
			if i < len(rs) {
				b.WriteRune(rs[i])
			}
		case c == '/' && i+1 < len(rs) && rs[i+1] == '*':
			depth := 0
			for i < len(rs) {
				if rs[i] == '/' && i+1 < len(rs) && rs[i+1] == '*' {
					depth++
					b.WriteString("/*")
					i += 2
					continue
				}
				if rs[i] == '*' && i+1 < len(rs) && rs[i+1] == '/' {
					depth--
					b.WriteString("*/")
					i += 2
					if depth == 0 {
						break
					}
					continue
				}
				b.WriteRune(rs[i])
				i++
			}
			i--
		case c == '\'' || c == '"':
			escapes := c == '\'' && i > 0 && (rs[i-1] == 'E' || rs[i-1] == 'e') && (i < 2 || !isIdentRune(rs[i-2]))
			b.WriteRune(c)
			for i++; i < len(rs); i++ {
				b.WriteRune(rs[i])
				if escapes && rs[i] == '\\' && i+1 < len(rs) {
					i++
					b.WriteRune(rs[i])
					continue
				}
				if rs[i] == c {
					if i+1 < len(rs) && rs[i+1] == c { // doubled quote
						i++
						b.WriteRune(rs[i])
						continue
					}
					break
				}
			}
		case c == '$' && (i == 0 || !isIdentRune(rs[i-1])):
			if tag, ok := dollarTag(rs, i); ok {
				b.WriteString(tag)
				i += utf8.RuneCountInString(tag)
				end := strings.Index(string(rs[i:]), tag)
				if end < 0 {
					b.WriteString(string(rs[i:]))
					i = len(rs)
					break
				}
				body := string(rs[i:])[:end]
				b.WriteString(body + tag)
				i += utf8.RuneCountInString(body) + utf8.RuneCountInString(tag) - 1
			} else {
				b.WriteRune(c)
			}
		case c == ';':
			flush()
		default:
			b.WriteRune(c)
		}
	}
	flush()
	return out
}

func isIdentRune(r rune) bool {
	return r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r > 127
}

// dollarTag returns "$tag$" if one starts at rs[i].
func dollarTag(rs []rune, i int) (string, bool) {
	for j := i + 1; j < len(rs); j++ {
		if rs[j] == '$' {
			return string(rs[i : j+1]), true
		}
		if !(rs[j] == '_' || (rs[j] >= 'a' && rs[j] <= 'z') || (rs[j] >= 'A' && rs[j] <= 'Z') || (j > i+1 && rs[j] >= '0' && rs[j] <= '9') || rs[j] > 127) {
			return "", false
		}
	}
	return "", false
}

var sqlCommentRE = regexp.MustCompile(`(?s)--[^\n]*|/\*.*?\*/`)

func stripSQLComments(s string) string { return sqlCommentRE.ReplaceAllString(s, " ") }

// refusedLeadingWords end or escape the read-only transaction, touch files or
// programs, or take locks / do maintenance that a console must not trigger.
var refusedLeadingWords = map[string]string{
	"begin": "transaction control", "start": "transaction control", "commit": "transaction control",
	"end": "transaction control", "rollback": "transaction control", "abort": "transaction control",
	"savepoint": "transaction control", "release": "transaction control",
	"copy": "COPY reads or writes files and programs", "lock": "explicit table locks",
	"vacuum": "maintenance", "analyze": "maintenance", "cluster": "maintenance", "reindex": "maintenance",
	"checkpoint": "maintenance", "refresh": "maintenance", "discard": "session reset",
	"listen": "notifications", "notify": "notifications", "unlisten": "notifications",
}

// refusedFunctions act on other sessions, the server, replication, files, or
// reach other databases — none of which a read-only transaction prevents.
var refusedFunctions = []string{
	"pg_terminate_backend", "pg_cancel_backend", "pg_reload_conf", "pg_rotate_logfile", "pg_switch_wal",
	"pg_promote", "pg_create_restore_point", "pg_wal_replay_pause", "pg_wal_replay_resume",
	"pg_backup_start", "pg_backup_stop", "pg_start_backup", "pg_stop_backup",
	"pg_create_physical_replication_slot", "pg_create_logical_replication_slot", "pg_drop_replication_slot",
	"pg_replication_slot_advance", "pg_copy_physical_replication_slot", "pg_copy_logical_replication_slot",
	"pg_logical_emit_message", "pg_stat_reset", "pg_stat_statements_reset",
	"lo_import", "lo_export", "lo_unlink", "pg_file_write", "pg_file_rename", "pg_file_unlink", "pg_file_sync",
	"dblink", "pg_background",
}

var leadingWordRE = regexp.MustCompile(`^[\s(]*([A-Za-z]+)(?:\s+([A-Za-z]+))?`)

// refuseStatement returns why a statement may not run, or "" if it may.
func refuseStatement(stmt string) string {
	s := stripSQLComments(stmt)
	if m := leadingWordRE.FindStringSubmatch(s); m != nil {
		first, second := strings.ToLower(m[1]), strings.ToLower(m[2])
		if first == "prepare" && second == "transaction" {
			return "PREPARE TRANSACTION is transaction control, which pgbot doesn't run"
		}
		if first == "set" && (second == "session" || second == "role" || second == "session_authorization") {
			return "changing the session's role or characteristics isn't allowed"
		}
		if why, ok := refusedLeadingWords[first]; ok {
			return fmt.Sprintf("%s (%s) isn't allowed in pgbot's read-only console", strings.ToUpper(first), why)
		}
	}
	lower := strings.ToLower(s)
	for _, fn := range refusedFunctions {
		if strings.Contains(lower, fn) {
			return fmt.Sprintf("%s acts outside this read-only transaction, so pgbot won't call it", fn)
		}
	}
	return ""
}
