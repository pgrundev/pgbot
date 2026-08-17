package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"github.com/pgrundev/pgbot/internal/mcp"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/render"
	"github.com/pgrundev/pgbot/internal/store"
	"github.com/spf13/cobra"
)

// newMCPCmd runs pgbot as a Model Context Protocol server over stdio, so an AI
// agent can call it as a tool. It exposes DETERMINISTIC tools only — the agent
// (the model) does the explaining over the findings pgbot returns.
func newMCPCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run as an MCP server over stdio (for AI agents like Claude)",
		Long: "Speaks the Model Context Protocol on stdin/stdout. Configure it in an MCP\n" +
			"client (Claude Desktop/Code, Cursor, …) and the agent gains read-only tools:\n" +
			"  inspect         — full health findings as JSON\n" +
			"  unused_indexes  — zero-scan indexes + the replication caveat\n\n" +
			"Set $DATABASE_URL in the server's env so tools need no connection argument,\n" +
			"or pass connection_string per call. pgbot never writes to the database.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			srv := &mcp.Server{
				Name:    "pgbot",
				Version: version,
				Instructions: "pgbot gives read-only PostgreSQL health findings. Call `inspect` and " +
					"explain its findings to the user — the findings are computed deterministically; " +
					"treat them as facts and carry any caveats into your advice.",
				Tools:     pgbotTools(),
				Prompts:   pgbotPrompts(),
				Resources: pgbotResources(),
			}
			fmt.Fprintln(os.Stderr, "pgbot mcp: serving on stdio (ctrl-c to stop)")
			return srv.Serve(cmd.Context(), os.Stdin, os.Stdout)
		},
	}
}

func pgbotTools() []mcp.Tool {
	dsnSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"connection_string": map[string]any{
				"type":        "string",
				"description": "postgres:// URL or libpq DSN. Optional if $DATABASE_URL is set for the server.",
			},
		},
	}
	return []mcp.Tool{
		{
			Name: "inspect",
			Description: "Run a read-only health inspection of a PostgreSQL database and return the " +
				"findings (critical/warning/note), a health score, wait-event profile, unused " +
				"indexes, and key stats as JSON. Deterministic — computed in Go, not by a model. " +
				"pgbot never writes to the database.",
			InputSchema: dsnSchema,
			Handler:     inspectTool,
		},
		{
			Name: "unused_indexes",
			Description: "List indexes with zero scans in the observed window (schema, table, name, " +
				"bytes), plus whether replication is active — because on a primary these counts are " +
				"per-node and a replica may still use an index that looks unused here. Read-only.",
			InputSchema: dsnSchema,
			Handler:     unusedIndexesTool,
		},
		{
			Name: "top_queries",
			Description: "Top statements from pg_stat_statements ranked by cumulative total execution " +
				"time, each with its share of total DB exec time (share_pct), call count, and mean ms. " +
				"Answers 'which query is eating the database.' Query text is normalized ($1 placeholders) " +
				"— no literals. Transaction-control/SET noise is filtered. Read-only.",
			InputSchema: dsnSchema,
			Handler:     topQueriesTool,
		},
		{
			Name: "vacuum_health",
			Description: "Autovacuum health per table, ranked by dead tuples: live/dead tuple counts, " +
				"dead ratio, last autovacuum time, and a computed 'due' flag (dead tuples past Postgres' " +
				"default autovacuum trigger of 50 + 20% of live rows). Read-only.",
			InputSchema: dsnSchema,
			Handler:     vacuumHealthTool,
		},
	}
}

// pgbotPrompts offers one-click workflows an agent can invoke.
func pgbotPrompts() []mcp.Prompt {
	return []mcp.Prompt{{
		Name:        "diagnose",
		Description: "Inspect the database and produce a prioritized, plain-language diagnosis.",
		Arguments: []mcp.PromptArg{
			{Name: "connection_string", Description: "postgres:// URL or DSN (optional if $DATABASE_URL is set)", Required: false},
		},
		Build: func(_ context.Context, args map[string]string) ([]mcp.PromptMessage, error) {
			call := "Call the pgbot `inspect` tool"
			if dsn := args["connection_string"]; dsn != "" {
				call += " with connection_string \"" + dsn + "\""
			}
			text := call + ", then give me a prioritized diagnosis: a one-line health verdict, then " +
				"each issue worst-first with a likely cause (only if the changes/events support one) and a " +
				"safe recommended step. pgbot's findings are computed deterministically — treat them as " +
				"facts, hedge anything below 0.5 confidence, and carry every caveat into your advice. pgbot " +
				"never writes, so recommend, don't act."
			return []mcp.PromptMessage{{Role: "user", Text: text}}, nil
		},
	}}
}

// pgbotResources exposes the local baseline store as a readable resource, so an
// agent can see which databases pgbot has history for.
func pgbotResources() []mcp.Resource {
	return []mcp.Resource{{
		URI:         "pgbot://baselines",
		Name:        "pgbot baselines",
		Description: "Databases pgbot has local baseline history for (fingerprint, database, snapshot count, time span).",
		MimeType:    "application/json",
		Read: func(_ context.Context) (string, error) {
			st, err := store.Open("")
			if err != nil {
				return "", err
			}
			defer st.Close()
			items, err := st.List()
			if err != nil {
				return "", err
			}
			b, err := json.MarshalIndent(items, "", "  ")
			if err != nil {
				return "", err
			}
			return string(b), nil
		},
	}}
}

// dsnFromArgs pulls connection_string from the tool arguments, falling back to
// the server's environment.
func dsnFromArgs(args json.RawMessage) (string, error) {
	var a struct {
		ConnectionString string `json:"connection_string"`
	}
	_ = json.Unmarshal(args, &a)
	dsn := firstNonEmpty(a.ConnectionString, os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"))
	if dsn == "" {
		return "", fmt.Errorf("no connection string: pass connection_string or set $DATABASE_URL for the server")
	}
	return dsn, nil
}

func inspectTool(ctx context.Context, args json.RawMessage) (string, error) {
	dsn, err := dsnFromArgs(args)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	c, _, err := gather(ctx, dsn, inspectFlags{interval: time.Second, ashHz: 10, window: 5 * time.Second})
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := render.JSON(&buf, c); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func unusedIndexesTool(ctx context.Context, args json.RawMessage) (string, error) {
	dsn, err := dsnFromArgs(args)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// No wait sampling needed for an index listing.
	c, _, err := gather(ctx, dsn, inspectFlags{interval: time.Second, ashHz: 0, noStore: true})
	if err != nil {
		return "", err
	}
	out := map[string]any{
		"replication_active": c.Replication != nil && (len(c.Replication.Replicas) > 0 || c.Replication.IsReplica),
		"cold_window":        c.Window.ColdWindow(),
		"unused":             []any{},
	}
	if c.Indexes != nil {
		out["unused"] = c.Indexes.Unused
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// topQueriesTool returns the pg_stat_statements top-N by total execution time,
// each carrying its share of total DB exec time — the agent-facing counterpart
// to the `pgbot queries` command.
func topQueriesTool(ctx context.Context, args json.RawMessage) (string, error) {
	dsn, err := dsnFromArgs(args)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, _, err := gather(ctx, dsn, inspectFlags{interval: time.Second, ashHz: 0, noStore: true})
	if err != nil {
		return "", err
	}
	out := map[string]any{"enabled": false, "ranked_by": "total_exec_time", "queries": []any{}}
	switch {
	case c.Queries != nil && c.Queries.Enabled:
		out["enabled"] = true
		out["total_exec_ms"] = c.Queries.TotalExecMS
		rows := make([]map[string]any, 0, len(c.Queries.Top))
		for _, q := range c.Queries.Top {
			share := 0.0
			if c.Queries.TotalExecMS > 0 {
				share = math.Round(q.TotalMS/c.Queries.TotalExecMS*1000) / 10
			}
			rows = append(rows, map[string]any{
				"query": q.Query, "calls": q.Calls,
				"total_ms": q.TotalMS, "mean_ms": q.MeanMS, "share_pct": share,
			})
		}
		out["queries"] = rows
	case c.Queries != nil && c.Queries.Reason != "":
		out["reason"] = c.Queries.Reason
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// vacuumHealthTool returns per-table autovacuum health ranked by dead tuples,
// with a computed "due" flag — the agent-facing counterpart to `pgbot vacuum`.
func vacuumHealthTool(ctx context.Context, args json.RawMessage) (string, error) {
	dsn, err := dsnFromArgs(args)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, _, err := gather(ctx, dsn, inspectFlags{interval: time.Second, ashHz: 0, noStore: true})
	if err != nil {
		return "", err
	}
	out := map[string]any{"tables_past_threshold": 0, "tables": []any{}}
	if c.Tables == nil {
		b, _ := json.MarshalIndent(out, "", "  ")
		return string(b), nil
	}
	tbls := append([]model.TableStat(nil), c.Tables.Top...)
	sort.SliceStable(tbls, func(i, j int) bool { return tbls[i].DeadTuples > tbls[j].DeadTuples })
	past := 0
	rows := make([]map[string]any, 0, len(tbls))
	for _, t := range tbls {
		due := expectAutovacuum(t, avVacThreshold(c), avVacScale(c))
		if due {
			past++
		}
		row := map[string]any{
			"table": t.Schema + "." + t.Name, "live_tuples": t.LiveTuples,
			"dead_tuples": t.DeadTuples, "dead_ratio": t.DeadRatio, "due": due,
		}
		if t.LastAutovac != nil {
			row["last_autovacuum"] = t.LastAutovac.UTC().Format(time.RFC3339)
		}
		rows = append(rows, row)
	}
	out["tables_past_threshold"] = past
	out["tables"] = rows
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
