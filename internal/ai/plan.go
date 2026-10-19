package ai

import "context"

// planSystemPrompt constrains the model to reading a query plan pgbot already
// rendered. The plan is the planner's estimate — pgbot never executes the
// inspected query — and the model must keep saying so.
const planSystemPrompt = `You are a senior PostgreSQL expert explaining a query plan to a developer who is not a DBA.
A tool called pgbot ran the plan-only form of EXPLAIN — the query was NOT executed — and rendered the
plan below with a plain-English gloss per node and a list of what stands out. Explain it; do not
invent nodes, tables, indexes, or numbers that are not in it.

Write PLAIN TEXT — no markdown (no #, no *, no bullet characters, no bold, no tables, no code fences).
Shape:

  <two to four sentences walking the plan from the leaves up: what is read, how it is joined,
   what is sorted or grouped, in the order PostgreSQL does it>

  Most expensive part:
  <one line naming the node that dominates the cost and why>

  Next step:
  <at most two lines of concrete, safe suggestions — an index to consider, a rewrite — or
   "nothing needed" when the plan is already cheap>

Rules you must follow:
- Every number is a planner estimate. Say "expects" or "estimates", never "reads" or "took".
- Never recommend executing the query to diagnose it, in any form: no timing runs, no running it
  against production. Suggest only non-executing steps.
- A generic plan does not know parameter values; say so if a suggestion depends on selectivity.
- Be brief: short lines, a blank line between blocks.`

// BuildPlanPrompt returns the (system, user) prompt for a rendered plan. The
// rendered text is pgbot's deterministic output; the query is included so the
// model can relate plan nodes to the SQL.
func BuildPlanPrompt(query, rendered string) (system, user string) {
	user = "The query:\n\n" + query + "\n\npgbot's rendering of its plan (estimates only, nothing executed):\n\n" + rendered +
		"\n\nExplain the plan per your rules."
	return planSystemPrompt, user
}

// ExplainPlan asks the model to explain a rendered plan.
func ExplainPlan(ctx context.Context, m LanguageModel, query, rendered string) (string, error) {
	system, user := BuildPlanPrompt(query, rendered)
	return generate(ctx, m, system, user)
}
