package advisor

import (
	"context"
	"strings"
)

// Planner is the minimal database surface the advisor drives. Implemented over a
// pgx READ ONLY transaction in the command; mocked in tests. Every method is a
// plan-only or hypothetical operation — none executes the inspected query or
// writes anything.
type Planner interface {
	// GenericPlan returns EXPLAIN (GENERIC_PLAN, VERBOSE, FORMAT JSON) <query> as raw JSON.
	// GENERIC_PLAN (PG16+) plans a normalized $N query without values or execution.
	GenericPlan(ctx context.Context, query string) ([]byte, error)
	// CreateHypoIndex creates a hypothetical index from a CREATE INDEX statement
	// and returns its planner-visible name (hypopg_create_index) plus hypopg's
	// estimated on-disk size in bytes. Nothing is built.
	CreateHypoIndex(ctx context.Context, ddl string) (name string, estBytes int64, err error)
	// ResetHypo drops every hypothetical index in this session (hypopg_reset).
	ResetHypo(ctx context.Context) error
}

// QueryInput is one candidate-for-improvement slow query.
type QueryInput struct {
	QueryID  int64
	Text     string // RAW normalized ($1) SQL, used only to plan — never stored
	Scrubbed string // display form (scrubbed), safe for output
	Calls    int64
	SharePct float64 // % of total DB exec time
}

// Options tunes the advisor.
type Options struct {
	MinImprovement float64 // required fractional cost drop, e.g. 0.5 = 50%
	// StaleRelations (schema.table) have stale or missing planner statistics, so a
	// cost estimate over them is untrustworthy — the advisor refuses to advise on
	// them and says why. WriteHeavy relations get a write-amplification caveat.
	StaleRelations map[string]bool
	WriteHeavy     map[string]bool
}

// Stats reports what the advisor examined, for an honest "nothing found" message.
type Stats struct {
	QueriesConsidered int
	QueriesPlanned    int // successfully planned (GENERIC_PLAN worked)
	CandidatesTested  int
	SkippedStaleStats int // candidates skipped because the table's stats are stale
}

// advisorConfidence is the ceiling on any recommendation's confidence: it is a
// planner cost ESTIMATE for a hypothetical index, never a measured speedup.
const advisorConfidence = 0.6

// Advise runs the deterministic-candidate → hypopg-validation loop over the given
// queries and returns only planner-confirmed recommendations. The caller is
// responsible for running this inside a single READ ONLY transaction (hypopg
// state is per-connection) and for the final ResetHypo — but Advise also resets
// between every candidate so plans never compound.
func Advise(ctx context.Context, p Planner, queries []QueryInput, opts Options) ([]Recommendation, Stats) {
	if opts.MinImprovement <= 0 {
		opts.MinImprovement = 0.5
	}
	var recs []Recommendation
	var st Stats

	for _, q := range queries {
		st.QueriesConsidered++
		clean := sanitizeQuery(q.Text)
		if clean == "" {
			continue
		}
		baseJS, err := p.GenericPlan(ctx, clean)
		if err != nil {
			continue // can't plan it (temp tables, unsupported params) — skip, don't fail
		}
		base, err := parsePlan(baseJS)
		if err != nil {
			continue
		}
		st.QueriesPlanned++
		costBefore := planCost(base)
		if costBefore <= 0 {
			continue
		}
		for _, cand := range candidatesFromPlan(base) {
			// Refuse to advise on a table whose statistics are stale: the planner's
			// cost estimate — and therefore the whole recommendation — is only as good
			// as the stats it's computed from.
			if opts.StaleRelations[cand.Schema+"."+cand.Table] {
				st.SkippedStaleStats++
				continue
			}
			st.CandidatesTested++
			if rec, ok := validate(ctx, p, q, cand, clean, costBefore, opts); ok {
				recs = append(recs, rec)
			}
		}
	}
	return dedupeRecommendations(recs), st
}

// validate creates one hypothetical index, re-plans, and confirms the planner
// switches to it with a real cost drop. It always resets hypopg afterward so the
// next candidate is tested in isolation.
func validate(ctx context.Context, p Planner, q QueryInput, cand Candidate, query string, costBefore float64, opts Options) (Recommendation, bool) {
	name, estBytes, err := p.CreateHypoIndex(ctx, cand.DDL())
	if err != nil || name == "" {
		return Recommendation{}, false // bad DDL / nonexistent column — hypopg rejected it
	}
	defer p.ResetHypo(ctx) //nolint:errcheck // reset is best-effort; the tx is read-only

	afterJS, err := p.GenericPlan(ctx, query)
	if err != nil {
		return Recommendation{}, false
	}
	after, err := parsePlan(afterJS)
	if err != nil {
		return Recommendation{}, false
	}
	costAfter := planCost(after)

	// Two independent gates: the planner must actually PICK the hypothetical index
	// (name match), AND the estimated total cost must fall by the threshold. Either
	// alone is too weak — cost can wobble, and an unused index proves nothing.
	if !usesIndex(after, name) || costAfter >= costBefore*(1-opts.MinImprovement) {
		return Recommendation{}, false
	}
	rec := Recommendation{
		Candidate: cand, IndexDDL: cand.DDL(),
		Schema: cand.Schema, Table: cand.Table, Columns: cand.Columns,
		QueryID: q.QueryID, QueryText: q.Scrubbed, Calls: q.Calls, SharePct: q.SharePct,
		CostBefore: costBefore, CostAfter: costAfter, EstimatedBytes: estBytes,
		Confidence: advisorConfidence,
	}
	// The load-bearing caveats every recommendation carries.
	rec.Caveats = append(rec.Caveats,
		"planner estimate, not a measured speedup — verify on a copy before building",
		"build with CREATE INDEX CONCURRENTLY off-peak; a plain CREATE INDEX locks the table against writes")
	if opts.WriteHeavy[cand.Schema+"."+cand.Table] {
		rec.Caveats = append(rec.Caveats,
			"this table is write-heavy — the index is maintained on every INSERT/UPDATE; weigh the read gain against the added write cost (cf. low_hot_update_ratio)")
	}
	return rec, true
}

// SanitizeSelect is the exported guard for any surface that EXPLAINs
// agent-supplied SQL (the MCP explain_plan tool): it returns the trimmed query if
// it is a single plain SELECT safe to plan, or "" to refuse. Same rules as the
// advisor's own path — SELECT-only, no interior semicolon.
func SanitizeSelect(s string) string { return sanitizeQuery(s) }

// sanitizeQuery trims a single normalized statement for EXPLAIN and refuses
// anything that could smuggle a second statement or a write past the planner.
// GenericPlan uses the simple-query protocol (PgConn.Exec), which WOULD run a
// ";"-separated second statement, so this is the first of the two layers that
// keep §0.1 ("never execute the inspected query") true — the READ ONLY
// transaction is the second (see TestAdviseSafety_readOnlyTxBlocksInjectedWrite).
//
// It rejects: anything not starting with SELECT (no WITH, which can hide a
// writable CTE; no DML; no utility statement, whose pgss text can carry
// literals), and ANY interior semicolon. We reject every ";" rather than only
// statement separators "outside literals and comments" because the input is
// normalized pg_stat_statements text — its literals are already $N and its
// comments are stripped, so there is no literal or comment for a ";" to live in,
// which means any interior ";" IS a statement separator. Over-rejecting a rare
// query is safe (we just skip advising it); under-rejecting would let a write
// reach the server and lean entirely on the transaction being read-only.
func sanitizeQuery(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "; \t\n\r")
	if s == "" {
		return ""
	}
	if !startsWithWord(s, "select") {
		return ""
	}
	if strings.Contains(s, ";") {
		return "" // an interior ";" — a second statement snuck in; refuse
	}
	return s
}

// startsWithWord reports whether s begins with word followed by a non-identifier
// char (so "select" matches but "selection" doesn't), case-insensitively.
func startsWithWord(s, word string) bool {
	if len(s) < len(word) {
		return false
	}
	if !strings.EqualFold(s[:len(word)], word) {
		return false
	}
	if len(s) == len(word) {
		return true
	}
	c := s[len(word)]
	return !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'))
}
