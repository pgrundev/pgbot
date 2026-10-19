package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/pgrundev/pgbot/internal/ai"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/plan"
	"github.com/pgrundev/pgbot/internal/render"
	"github.com/spf13/cobra"
)

// newSQLCmd groups the statement-level tools. `pgbot sql explain` is the first:
// the planner's plan for one SELECT, read back in plain English. Cobra matches
// command names case-insensitively (see main), so `pgbot SQL EXPLAIN` works too.
func newSQLCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sql",
		Short: "Statement-level tools: explain a query's plan in plain language",
		Long: "Tools that take one SQL statement rather than inspecting the whole database.\n" +
			"Everything here is read-only: a statement is planned, never executed.",
	}
	cmd.AddCommand(newSQLExplainCmd())
	return cmd
}

type sqlExplainFlags struct {
	jsonOut bool
	useAI   bool
	yes     bool
	noColor bool
}

func newSQLExplainCmd() *cobra.Command {
	var f sqlExplainFlags
	cmd := &cobra.Command{
		Use:   `explain [connection-string] "<SELECT …>"`,
		Short: "Show a SELECT's query plan and read it in plain English",
		Long: "Runs the plan-only form of EXPLAIN for one SELECT — the query is planned inside a\n" +
			"READ ONLY transaction and never executed — and renders the plan as a tree with a\n" +
			"plain-English line per node, what stands out (sequential scans on large tables,\n" +
			"nested loops that repeat a scan, sorts an index could avoid), and the caveat that\n" +
			"every number is the planner's estimate.\n\n" +
			"On PostgreSQL 16+ the plan is generic (EXPLAIN GENERIC_PLAN), so $1-style\n" +
			"placeholders need no values. Only a single plain SELECT is accepted: no writes,\n" +
			"no WITH, no multiple statements.\n\n" +
			"The query is the last argument, or stdin when piped. The connection is the first\n" +
			"argument, else $DATABASE_URL / $PGBOT_DATABASE_URL / $PGSERVICE.\n\n" +
			"--ai adds a model's prose explanation of the rendered plan. Unlike pgbot's\n" +
			"findings, your SQL text can contain literal values, so the disclosure prompt\n" +
			"names exactly what is sent: the query and its plan.",
		Example: "  pgbot sql explain \"SELECT * FROM orders WHERE customer_id = $1\"\n" +
			"  pgbot sql explain \"$DATABASE_URL\" \"SELECT count(*) FROM events WHERE kind = 'click'\"\n" +
			"  cat slow.sql | pgbot sql explain --ai",
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSQLExplain(cmd, args, f)
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.jsonOut, "json", false, "print the raw EXPLAIN (FORMAT JSON) output instead of the reading")
	fl.BoolVar(&f.useAI, "ai", false, "append a model's plain-language explanation (needs an AI key in the environment)")
	fl.BoolVar(&f.yes, "yes", false, "skip the data-disclosure confirmation prompt (--ai)")
	fl.BoolVar(&f.noColor, "no-color", false, "disable ANSI color")
	return cmd
}

// resolveSQLExplainInput works out the connection string and the query from the
// positional arguments, the environment and (when piped) stdin:
//
//	explain "<sql>"              → query, connection from env
//	explain <conn> "<sql>"       → both explicit
//	explain < file.sql           → query from stdin, connection from env
//	explain <conn> < file.sql    → connection explicit, query from stdin
func resolveSQLExplainInput(args []string, stdin io.Reader, stdinIsTerminal bool) (connString, query string, err error) {
	switch len(args) {
	case 2:
		connString, query = args[0], args[1]
	case 1:
		if looksLikeConnString(args[0]) {
			connString = args[0]
		} else {
			query = args[0]
		}
	}
	if query == "" {
		if stdinIsTerminal {
			return "", "", usageErrf("no query given — pass a SELECT as the last argument or pipe it on stdin")
		}
		b, rerr := io.ReadAll(stdin)
		if rerr != nil {
			return "", "", fmt.Errorf("read stdin: %w", rerr)
		}
		query = strings.TrimSpace(string(b))
		if query == "" {
			return "", "", usageErrf("no query given — stdin was empty")
		}
	}
	connString = firstNonEmpty(connString, os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"), pgServiceFallback())
	if connString == "" {
		return "", "", fmt.Errorf("no connection string (pass one before the query or set $DATABASE_URL)")
	}
	return connString, query, nil
}

// looksLikeConnString tells a connection string from a SQL statement: a
// postgres:// URL, or a libpq keyword string (host=…, dbname=…, service=…).
func looksLikeConnString(s string) bool {
	s = strings.TrimSpace(s)
	low := strings.ToLower(s)
	if strings.HasPrefix(low, "postgres://") || strings.HasPrefix(low, "postgresql://") {
		return true
	}
	for _, kw := range []string{"host=", "dbname=", "service=", "user=", "port=", "hostaddr="} {
		if strings.HasPrefix(low, kw) {
			return true
		}
	}
	return false
}

func runSQLExplain(cmd *cobra.Command, args []string, f sqlExplainFlags) error {
	connString, query, err := resolveSQLExplainInput(args, os.Stdin, isInteractive())
	if err != nil {
		return err
	}
	// Refuse anything but one plain SELECT before connecting; a bad statement is
	// a usage error (exit 64), not an execution failure.
	if _, err := plan.Prepare(query, 160000); err != nil {
		if errors.Is(err, plan.ErrNotSelect) {
			return usageErrf("%s", err.Error())
		}
		return err
	}

	// --ai: resolve the model and get consent BEFORE connecting, so a missing key
	// fails fast and the user knows exactly what leaves the machine.
	var llm ai.LanguageModel
	if f.useAI {
		llm, err = ai.Resolve()
		if err != nil {
			return err
		}
		if !confirmDisclosureOf("pgbot sql explain", "your SQL text and its query plan", llm, f.yes) {
			return fmt.Errorf("aborted")
		}
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	target, err := conn.Connect(ctx, connString)
	if err != nil {
		return fmt.Errorf("connect: %s", conn.RedactConnString(err.Error()))
	}
	defer target.Close()
	if target.Pooler.Detected {
		fmt.Fprintln(os.Stderr, target.Pooler.Note())
	}

	planJSON, generic, err := plan.Fetch(ctx, target, query)
	if err != nil {
		return err
	}
	if f.jsonOut {
		var pretty bytes.Buffer
		if json.Indent(&pretty, planJSON, "", "  ") != nil {
			pretty.Write(planJSON)
		}
		fmt.Println(pretty.String())
		return nil
	}
	root, err := plan.Parse(planJSON)
	if err != nil {
		return err
	}

	color := useColor(f.noColor)
	// The generic-plan caveat matters only when there were parameters for the
	// planner not to know.
	in := render.PlanInput{Query: query, Root: root, Observations: plan.Observe(root), Generic: generic && plan.HasPlaceholders(query), Color: color}
	render.Plan(os.Stdout, in)

	if llm != nil {
		// The model reads the same text the user just saw, uncolored.
		var plain bytes.Buffer
		in.Color = false
		render.Plan(&plain, in)
		aiCtx, aiCancel := context.WithTimeout(cmd.Context(), 3*time.Minute)
		defer aiCancel()
		text, aiErr := ai.ExplainPlan(aiCtx, llm, query, plain.String())
		printAISection(color, llm.Model(), text, aiErr)
	}
	return nil
}
