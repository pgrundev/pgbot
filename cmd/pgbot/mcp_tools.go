package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pgrundev/pgbot/docs"
	"github.com/pgrundev/pgbot/internal/advisor"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/store"
	"github.com/pgrundev/pggo"
)

// B8 MCP tools. Every one either produces no findings (explain_plan, schema_of,
// explain_finding) or reuses a path that already honors B2 suppression
// (compare_to_baseline over stored snapshots). Responses carry exactness/caveat
// labels in the PAYLOAD: the guarantee is that the "estimate, not measurement"
// qualifier is present in the response, not that the consuming model preserves it
// — a third-party client can still reword or drop it on its side.

// explainPlanTool runs a PLAIN EXPLAIN (the plan-only form) of an agent-supplied query
// through the same guard as the advisor: SanitizeSelect (single plain SELECT, no
// semicolons) plus a READ ONLY transaction. The query is planned, never executed.
func explainPlanTool(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		ConnectionString string `json:"connection_string"`
		Query            string `json:"query"`
	}
	_ = json.Unmarshal(args, &a)
	if strings.TrimSpace(a.Query) == "" {
		return "", fmt.Errorf("query is required")
	}
	clean := advisor.SanitizeSelect(a.Query)
	if clean == "" {
		return "", fmt.Errorf("only a single plain SELECT can be explained — no writes, no multiple statements, no utility statements")
	}
	dsn, err := dsnFromArgs(args)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	target, err := conn.Connect(ctx, dsn)
	if err != nil {
		return "", fmt.Errorf("connect: %s", conn.RedactConnString(err.Error()))
	}
	defer target.Close()

	// GENERIC_PLAN (PG16+) plans a query with $N placeholders without values;
	// below 16, plain EXPLAIN works for a query with literal values and errors
	// clearly on one with placeholders.
	stmt := "EXPLAIN (FORMAT JSON) " + clean
	if target.Caps.VersionNum >= 160000 {
		stmt = "EXPLAIN (GENERIC_PLAN, FORMAT JSON) " + clean
	}
	var planJSON []byte
	err = target.ReadOnlyTx(ctx, func(tx *pggo.Tx) error {
		res, e := tx.Conn().SimpleQuery(ctx, stmt)
		if e != nil {
			return e
		}
		for _, r := range res {
			if len(r.Rows) > 0 && len(r.Rows[0]) > 0 {
				planJSON = r.Rows[0][0]
				return nil
			}
		}
		return fmt.Errorf("no plan returned")
	})
	if err != nil {
		return "", fmt.Errorf("explain: %s", conn.RedactConnString(err.Error()))
	}
	return marshal(map[string]any{
		"plan":      json.RawMessage(planJSON),
		"exactness": "estimate",
		"note":      "plain EXPLAIN — the query was planned, NOT executed. Costs and row counts are the planner's ESTIMATES, not measurements; state them as such.",
	})
}

// compareToBaselineTool is `pgbot diff` as a tool: it reads the local baseline
// store (no connection) and returns the change set with the same interval honesty
// and reset/eviction caveats a human gets — which an agent needs more, since it
// would otherwise report a delta as fact.
func compareToBaselineTool(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Fingerprint  string `json:"fingerprint"`
		SinceSeconds int64  `json:"since_seconds"`
	}
	_ = json.Unmarshal(args, &a)
	since := 24 * time.Hour
	if a.SinceSeconds > 0 {
		since = time.Duration(a.SinceSeconds) * time.Second
	}
	r, err := resolveDiff("", a.Fingerprint, since)
	if err != nil {
		return "", err
	}
	out := diffJSON(r)
	out["note"] = "compared the interval actually available (actual_seconds), not necessarily the one requested; if stats_reset_between is set the cumulative deltas are meaningless, and if pgss_evicted the query-level deltas are incomplete — carry these caveats."
	return marshal(out)
}

// schemaOfTool returns a table's structure — columns, indexes, constraints, and
// the planner's row estimate — and NO data. It's the thing an agent needs before
// it can reason about a query.
func schemaOfTool(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		ConnectionString string `json:"connection_string"`
		Table            string `json:"table"`
	}
	_ = json.Unmarshal(args, &a)
	if strings.TrimSpace(a.Table) == "" {
		return "", fmt.Errorf("table is required (schema-qualified, e.g. public.orders)")
	}
	dsn, err := dsnFromArgs(args)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	target, err := conn.Connect(ctx, dsn)
	if err != nil {
		return "", fmt.Errorf("connect: %s", conn.RedactConnString(err.Error()))
	}
	defer target.Close()

	out := map[string]any{"table": a.Table, "exactness": "scraped", "note": "catalog metadata only — no table data is read; the row count is the planner's estimate (reltuples)."}
	// The table name is passed as a bind parameter cast to regclass — never spliced.
	err = target.ReadOnlyTx(ctx, func(tx *pggo.Tx) error {
		var relTuples int64
		var totalBytes int64
		if e := tx.QueryRow(ctx, `SELECT reltuples::bigint, pg_total_relation_size($1::regclass) FROM pg_class WHERE oid = $1::regclass`, a.Table).Scan(&relTuples, &totalBytes); e != nil {
			return e
		}
		out["estimated_rows"] = relTuples
		out["total_bytes"] = totalBytes
		out["columns"] = scanRows(ctx, tx, `SELECT a.attname AS name, format_type(a.atttypid, a.atttypmod) AS type, a.attnotnull AS not_null
			FROM pg_attribute a WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`, a.Table)
		out["indexes"] = scanRows(ctx, tx, `SELECT i.indexrelid::regclass::text AS name, i.indisunique AS unique, i.indisprimary AS primary,
			pg_get_indexdef(i.indexrelid) AS definition FROM pg_index i WHERE i.indrelid = $1::regclass`, a.Table)
		out["constraints"] = scanRows(ctx, tx, `SELECT conname AS name, contype AS type, pg_get_constraintdef(oid) AS definition
			FROM pg_constraint WHERE conrelid = $1::regclass`, a.Table)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("schema_of %s: %s", a.Table, conn.RedactConnString(err.Error()))
	}
	return marshal(out)
}

// explainFindingTool serves the B7 catalogue page for a finding, so an agent
// explains a recommendation with pgbot's words instead of inventing them.
func explainFindingTool(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(args, &a)
	if a.ID == "" {
		return "", fmt.Errorf("id is required (a finding id, e.g. unused_indexes)")
	}
	data, err := docs.Findings.ReadFile("findings/" + a.ID + ".md")
	if err != nil {
		return "", fmt.Errorf("no catalogue page for %q — pass a finding id from a report", a.ID)
	}
	return stripFrontMatter(string(data)), nil
}

// indexCorrelationTool runs a read-only collection and returns the index/code
// correlation payload — confidence levels plus, for the actionable ones, the
// identifiers to grep and how to interpret the result. pgbot never reads the repo.
func indexCorrelationTool(ctx context.Context, args json.RawMessage) (string, error) {
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
	// Default store path for verdict read-back; a missing store just means no history.
	return marshal(buildCorrelation(c, ""))
}

// recordIndexVerdictTool persists an agent's code-search verdict for one index.
// It writes only the local store — no database connection — so the agent can
// record what its repo grep found after index_code_correlation told it what to look for.
func recordIndexVerdictTool(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Fingerprint     string   `json:"fingerprint"`
		Index           string   `json:"index"`
		Verdict         string   `json:"verdict"`
		Source          string   `json:"source"`
		RepoRef         string   `json:"repo_ref"`
		StatsWindowDays *float64 `json:"stats_window_days"`
	}
	_ = json.Unmarshal(args, &a)
	if a.Fingerprint == "" || a.Index == "" || a.Verdict == "" {
		return "", fmt.Errorf("fingerprint, index, and verdict are required")
	}
	switch a.Verdict {
	case "found_in_code", "not_found_in_code", "inconclusive":
	default:
		return "", fmt.Errorf("verdict must be one of: found_in_code, not_found_in_code, inconclusive")
	}
	st, err := store.Open("")
	if err != nil {
		return "", err
	}
	defer st.Close()
	if err := st.SaveIndexVerdict(a.Fingerprint, store.IndexVerdict{
		IndexID: a.Index, Verdict: a.Verdict, Source: a.Source, RepoRef: a.RepoRef,
		CheckedAt: time.Now(), StatsWindowDays: a.StatsWindowDays,
	}); err != nil {
		return "", fmt.Errorf("record verdict: %w", err)
	}
	return marshal(map[string]any{
		"recorded": true, "fingerprint": a.Fingerprint, "index": a.Index, "verdict": a.Verdict,
		"note": "stored locally; a later index_code_correlation over a longer window will carry this forward as strengthening evidence.",
	})
}

// scanRows runs a query with one bind arg and returns its rows as []map, column
// names from the result description (best-effort; a query error yields nil).
func scanRows(ctx context.Context, tx *pggo.Tx, sql string, arg any) []map[string]any {
	rows, err := tx.Query(ctx, sql, arg)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []map[string]any
	fields := rows.Columns()
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return out
		}
		m := make(map[string]any, len(fields))
		for i, fd := range fields {
			m[fd.Name] = vals[i]
		}
		out = append(out, m)
	}
	return out
}

func marshal(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// whyTool runs the causal-chain analysis over the local baseline store — the
// agent-facing form of `pgbot why`. Offline like compare_to_baseline.
func whyTool(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Fingerprint   string `json:"fingerprint"`
		WindowSeconds int64  `json:"window_seconds"`
	}
	_ = json.Unmarshal(args, &a)
	window := 7 * 24 * time.Hour
	if a.WindowSeconds > 0 {
		window = time.Duration(a.WindowSeconds) * time.Second
	}
	report, err := computeWhy("", a.Fingerprint, window, 5)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return "", err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	out["note"] = "chains are computed deterministically from stored snapshot history — treat the numbers as facts; a confidence below 0.5 is a possibility, not a diagnosis, and the true cause may be outside what pgbot collects."
	return marshal(out)
}
