package ai

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pgrundev/pgbot/internal/model"
)

// systemPrompt hard-constrains the model to the one job it's allowed: explaining
// facts pgbot already computed, in a terse spoken-style shape. The findings are
// ground truth; the model may not add to them, and it MUST carry every caveat.
const systemPrompt = `You are a senior PostgreSQL expert giving a terse, spoken-style diagnosis to a
developer who is not a DBA. A tool called pgbot has already analyzed the database and computed the
findings below DETERMINISTICALLY — they are facts, not guesses. Explain them; never add new ones.

Write PLAIN TEXT in this exact shape — NO markdown (no #, no *, no bullet characters, no bold, no
tables, no code fences):

  <one sentence on overall health, e.g. "Your database is mostly healthy.">

  <then the findings that matter, worst first, at most three, each as a short block:>

  1 critical issue:          (the count and tier of this block — or "2 warnings:", etc.)
  <one line naming the problem, with the concrete number from the data>

  Likely cause:
  <one line — ONLY when the deltas or events actually support a cause; otherwise OMIT this
   "Likely cause:" block entirely. Never speculate a cause that the data does not show.>

  Recommended:
  <one line: a concrete, safe next step>

Rules you must follow:
- Every number comes from the data. Do NOT invent a multiplier, percentage, table, or query id.
- Each finding may carry "caveats". Carry them into the Recommended line — a caveat like
  "replication makes these per-node counts unreliable" means "verify on the replicas first" and
  is load-bearing. Never recommend a destructive action (dropping an index) without its caveat.
- A finding with confidence below 0.5 is a possibility — say "possibly" or "may", not a fact.
- Never recommend anything that EXECUTES a user's query to diagnose it (e.g. running it under
  timing); suggest only safe, non-executing steps.
- Be brief: short lines, a blank line between blocks. If the database is healthy, say so in one
  line and stop.`

// explainPayload is the PII-free subset of the Context we send. The full Context
// is already PII-free by construction (normalized query text, no literal values),
// but we send a focused view so the model reasons about signals, not noise.
type explainPayload struct {
	Database     string          `json:"database"`
	Version      string          `json:"version"`
	ViaPooler    bool            `json:"via_pooler,omitempty"`
	WindowAgeSec *int64          `json:"window_age_seconds,omitempty"`
	ColdWindow   bool            `json:"cold_window,omitempty"`
	Suppressed   string          `json:"delta_suppressed_reason,omitempty"`
	Findings     []model.Finding `json:"findings"`
	Waits        *waitSummary    `json:"wait_profile,omitempty"`
	Events       []eventSummary  `json:"recent_events,omitempty"`
	Deltas       *model.Deltas   `json:"changes_since_baseline,omitempty"` // temporal deltas — grounds causal claims
}

type waitSummary struct {
	Samples int                `json:"samples"`
	Buckets []model.WaitBucket `json:"top_buckets,omitempty"`
	ByQuery []model.QueryWaits `json:"top_queries,omitempty"`
}

type eventSummary struct {
	Kind       string  `json:"kind"`
	Object     string  `json:"object,omitempty"`
	Confidence float64 `json:"confidence"`
}

// payloadJSON is the curated, PII-free report we send. The full Context is
// already PII-free; this is a focused view so the model reasons about signals.
func payloadJSON(c *model.Context) string {
	p := explainPayload{
		Database:   c.Server.Database,
		Version:    c.Server.VersionText,
		ViaPooler:  c.Server.ViaPooler,
		ColdWindow: c.Window.ColdWindow(),
		Suppressed: c.DeltaSuppressedReason,
		Findings:   c.Findings,
	}
	if c.Window.WindowAgeSeconds != nil {
		p.WindowAgeSec = c.Window.WindowAgeSeconds
	}
	if w := c.WaitProfile; w != nil && w.Available && w.Samples > 0 {
		ws := &waitSummary{Samples: w.Samples}
		ws.Buckets = topBuckets(w.Buckets, 5)
		ws.ByQuery = topQueries(w.ByQuery, 3)
		p.Waits = ws
	}
	for _, e := range c.Events {
		p.Events = append(p.Events, eventSummary{Kind: e.Kind, Object: e.Object, Confidence: e.Confidence})
	}
	p.Deltas = c.Deltas // what changed vs the baseline — lets the model ground "3.2× slower"
	blob, _ := json.MarshalIndent(p, "", "  ")
	return string(blob)
}

// BuildExplainPrompt returns the (system, user) prompt for a Context. The user
// prompt is the curated JSON payload; nothing PII-bearing is included.
func BuildExplainPrompt(c *model.Context) (system, user string) {
	return systemPrompt, "Here is the pgbot report as JSON. Explain it per your rules.\n\n" + payloadJSON(c)
}

// BuildAskPrompt grounds a user's question in the same report. The model must
// answer ONLY from the report and its rules — it can't reach into the database.
func BuildAskPrompt(c *model.Context, question string) (system, user string) {
	user = "A user asked: \"" + question + "\"\n\n" +
		"Answer it using ONLY the pgbot report below and your rules. If the report doesn't\n" +
		"contain the answer, say what pgbot would need to collect to find out — do not guess.\n\n" +
		payloadJSON(c)
	return systemPrompt, user
}

// Explain builds the prompt and calls the model, returning the labeled-elsewhere
// explanation text. A nil/empty findings set still gets an explanation (the model
// is told to confirm health briefly).
func Explain(ctx context.Context, m LanguageModel, mc *model.Context) (string, error) {
	system, user := BuildExplainPrompt(mc)
	return generate(ctx, m, system, user)
}

// Ask answers a specific question grounded on the report.
func Ask(ctx context.Context, m LanguageModel, mc *model.Context, question string) (string, error) {
	system, user := BuildAskPrompt(mc, question)
	return generate(ctx, m, system, user)
}

// generate is the one place the explanation layer meets a provider. Low
// temperature because we want the model reading the findings, not riffing on
// them; 8192 output tokens because current models spend part of that budget
// thinking before the visible text.
func generate(ctx context.Context, m LanguageModel, system, user string) (string, error) {
	if m == nil {
		return "", fmt.Errorf("no model configured")
	}
	resp, err := m.Generate(ctx, Call{
		System:          system,
		Prompt:          user,
		Temperature:     f64(0.2),
		MaxOutputTokens: i64(8192),
	})
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}

func topBuckets(b []model.WaitBucket, n int) []model.WaitBucket {
	if len(b) > n {
		b = b[:n]
	}
	// Trim per-bucket event lists to the single top event to keep the prompt tight.
	out := make([]model.WaitBucket, 0, len(b))
	for _, bk := range b {
		if len(bk.Events) > 1 {
			bk.Events = bk.Events[:1]
		}
		out = append(out, bk)
	}
	return out
}

func topQueries(q []model.QueryWaits, n int) []model.QueryWaits {
	if len(q) > n {
		return q[:n]
	}
	return q
}
