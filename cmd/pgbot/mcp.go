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
				"The enabled flag reports extension presence; available reports successful collection. " +
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
		{
			Name: "suggest_indexes",
			Description: "Suggest missing indexes, each VALIDATED by the planner with hypopg — the " +
				"candidate is created hypothetically (nothing is built), the slow query re-planned, and " +
				"only surfaced if the planner switches to it with a real cost drop (cost_before/cost_after). " +
				"Deterministic candidates from the planner's own Seq Scan filters, never an LLM. Requires " +
				"hypopg + pg_stat_statements + PostgreSQL 16+; returns a reason when unavailable. pgbot " +
				"never runs your query and never builds an index — recommend, don't act.",
			InputSchema: dsnSchema,
			Handler:     suggestIndexesTool,
		},
		{
			Name: "explain_plan",
			Description: "Return the PLANNER'S plan for a SELECT (plain EXPLAIN, FORMAT JSON) — the " +
				"query is planned, never executed, in a read-only transaction. Only a single plain " +
				"SELECT is accepted (no writes, no multiple statements). Costs and row counts are " +
				"planner ESTIMATES, not measurements. The executing form of EXPLAIN is never used.",
			InputSchema: dsnSchemaWith(map[string]any{
				"query": map[string]any{"type": "string", "description": "A single plain SELECT to plan. May use $1 placeholders (planned generically on PG16+)."},
			}, "query"),
			Handler: explainPlanTool,
		},
		{
			Name: "compare_to_baseline",
			Description: "Diff a database's latest baseline snapshot against the one nearest `since_seconds` " +
				"ago, from the local store (no connection). Reports the interval it ACTUALLY compared " +
				"(actual_seconds) and flags a stats reset (stats_reset_between) or pg_stat_statements " +
				"eviction (pgss_evicted) that makes specific deltas untrustworthy — carry these caveats.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"fingerprint":   map[string]any{"type": "string", "description": "Which database (fingerprint or unique prefix). Required if the store holds more than one."},
					"since_seconds": map[string]any{"type": "integer", "description": "How far back to compare, in seconds (default 86400 = 24h)."},
				},
			},
			Handler: compareToBaselineTool,
		},
		{
			Name: "why",
			Description: "Explain a regression from stored baseline history: causal chains — symptom ← " +
				"mechanism ← antecedent (e.g. a query slowed BECAUSE seq scans surged on a table it reads " +
				"AFTER the table grew) — with the numbers and onset times for every hop. Computed " +
				"deterministically from snapshot history in the local store; no connection is made. " +
				"Needs at least 3 stored snapshots (each inspect adds one). Confidence below 0.5 is a " +
				"possibility, not a diagnosis.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"fingerprint":    map[string]any{"type": "string", "description": "Which database (fingerprint or unique prefix). Required if the store holds more than one."},
					"window_seconds": map[string]any{"type": "integer", "description": "How far back to analyze, in seconds (default 604800 = 7 days)."},
				},
			},
			Handler: whyTool,
		},
		{
			Name: "schema_of",
			Description: "Return a table's structure — columns (name/type/nullability), indexes, constraints, " +
				"and the planner's row estimate. NO table data is read. This is what an agent needs before " +
				"reasoning about a query. Read-only.",
			InputSchema: dsnSchemaWith(map[string]any{
				"table": map[string]any{"type": "string", "description": "Schema-qualified table name, e.g. public.orders."},
			}, "table"),
			Handler: schemaOfTool,
		},
		{
			Name: "explain_finding",
			Description: "Return pgbot's catalogue page for a finding id (what it observed, why it matters, " +
				"how to verify, how to fix, when to ignore, what pgbot cannot see) — so you explain a " +
				"recommendation with pgbot's words instead of inventing them.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": map[string]any{"type": "string", "description": "A finding id, e.g. unused_indexes."}},
				"required":   []string{"id"},
			},
			Handler: explainFindingTool,
		},
		{
			Name: "index_code_correlation",
			Description: "For each unused / redundant / invalid index, return a confidence level " +
				"(catalog_proven = safe to drop from the catalog alone; needs_code_check = actionable only with a " +
				"code search; inconclusive = do NOT drop on this evidence) and, for needs_code_check, the exact " +
				"identifiers to grep — camelCase, snake_case, PascalCase, CONSTANT_CASE — plus how to read the " +
				"result: search FILTER positions only (WHERE/JOIN/ORDER BY/GROUP BY), never SELECT lists. pgbot " +
				"never reads your repository; it tells you what to search for and how to interpret it. Also returns " +
				"a fingerprint to pass back to record_index_verdict. Read-only.",
			InputSchema: dsnSchema,
			Handler:     indexCorrelationTool,
		},
		{
			Name: "record_index_verdict",
			Description: "Record an agent's code-search verdict for one index (found_in_code | not_found_in_code | " +
				"inconclusive), keyed by the fingerprint from index_code_correlation. Stored locally so a later run " +
				"over a longer stats window carries strengthening evidence instead of starting over. No database " +
				"connection needed — this only writes pgbot's local store.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"fingerprint":       map[string]any{"type": "string", "description": "Database fingerprint from index_code_correlation."},
					"index":             map[string]any{"type": "string", "description": "Schema-qualified index, e.g. public.Job_externalIdNormalized_idx."},
					"verdict":           map[string]any{"type": "string", "description": "found_in_code | not_found_in_code | inconclusive."},
					"source":            map[string]any{"type": "string", "description": "Optional, e.g. agent_repo_search."},
					"repo_ref":          map[string]any{"type": "string", "description": "Optional commit sha the search ran against."},
					"stats_window_days": map[string]any{"type": "number", "description": "Optional: the stats_window_days you saw, so a later run can show the window has grown."},
				},
				"required": []string{"fingerprint", "index", "verdict"},
			},
			Handler: recordIndexVerdictTool,
		},
	}
}

// dsnSchemaWith returns the connection_string schema plus extra properties, with
// the named ones required.
func dsnSchemaWith(props map[string]any, required ...string) map[string]any {
	all := map[string]any{
		"connection_string": map[string]any{
			"type":        "string",
			"description": "postgres:// URL or libpq DSN. Optional if $DATABASE_URL is set for the server.",
		},
	}
	for k, v := range props {
		all[k] = v
	}
	return map[string]any{"type": "object", "properties": all, "required": required}
}

// pgbotPrompts offers one-click workflows an agent can invoke.
func pgbotPrompts() []mcp.Prompt {
	// No connection_string argument: prompt arguments are consumed here in
	// Build() and only the rendered text reaches the model, so the sole way an
	// argument could ever reach the inspect tool was by printing the DSN —
	// password included — into text that lands in transcripts and logs. The
	// inspect tool reads the server's own $DATABASE_URL instead, and the prompt
	// names that fix so a missing configuration doesn't turn into the model
	// asking the user to paste the DSN into chat.
	return []mcp.Prompt{{
		Name:        "diagnose",
		Description: "Inspect the database and produce a prioritized, plain-language diagnosis.",
		Build: func(_ context.Context, _ map[string]string) ([]mcp.PromptMessage, error) {
			text := "Call the pgbot `inspect` tool (it uses the DATABASE_URL configured on the pgbot " +
				"MCP server), then give me a prioritized diagnosis: a one-line health verdict, then " +
				"each issue worst-first with a likely cause (only if the changes/events support one) and a " +
				"safe recommended step. pgbot's findings are computed deterministically — treat them as " +
				"facts, hedge anything below 0.5 confidence, and carry every caveat into your advice. pgbot " +
				"never writes, so recommend, don't act. If inspect reports that no connection string is " +
				"configured, don't ask for one in chat (a DSN carries the password) — tell the user to set " +
				"DATABASE_URL on the pgbot MCP server instead."
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
	dsn := firstNonEmpty(a.ConnectionString, os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"), pgServiceFallback())
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
	// Carry the deterministic drop guard structurally — dropping an index is
	// destructive and the per-node/replica warning must not be lost to a summarizer.
	for _, f := range c.Findings {
		if f.ID == "unused_indexes" && f.Safety != nil {
			out["safety"] = f.Safety
			break
		}
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
	c, _, err := gatherQueryContext(ctx, dsn, inspectFlags{interval: time.Second, ashHz: 0, noStore: true})
	if err != nil {
		return "", err
	}
	out := map[string]any{"enabled": false, "available": false, "ranked_by": "total_exec_time", "queries": []any{}}
	switch {
	case c.Queries == nil:
	case !c.Queries.Enabled:
		if c.Queries.Reason != "" {
			out["reason"] = c.Queries.Reason
		}
	case c.Queries.Exactness == model.ExactnessUnavailable:
		out["enabled"] = true
		out["reason"] = queryStatsReadFailed
	default:
		out["enabled"] = true
		out["available"] = true
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
	gThreshold, gScale := avVacThreshold(c), avVacScale(c)
	past := 0
	rows := make([]map[string]any, 0, len(tbls))
	for _, t := range tbls {
		due := expectAutovacuum(t, gThreshold, gScale)
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
