// Package advisor is pgbot's evidential index advisor (B1). It NEVER guesses:
// candidate indexes are generated deterministically in Go by reading the
// planner's own Seq Scan filters, then each candidate is VALIDATED against
// PostgreSQL — created HYPOTHETICALLY with hypopg (nothing is built on disk),
// re-planned, and surfaced only if the planner actually switches to it and the
// estimated cost drops. "The advisor proposes; PostgreSQL's planner validates."
//
// Safety (see §0): every statement runs inside a READ ONLY transaction; the only
// query form used is EXPLAIN (GENERIC_PLAN) — plans a normalized pg_stat_statements
// query WITHOUT values and WITHOUT executing it (PG16+). The executing variant of
// EXPLAIN (the one that runs the query) is never used — the tree-wide CI guard
// forbids it. hypopg indexes live in backend memory and are dropped by hypopg_reset.
// pgbot never runs the inspected query itself.
package advisor

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Candidate is a proposed index: a table and an ordered column list.
type Candidate struct {
	Schema  string
	Table   string
	Columns []string // equality columns first, then range columns
}

// DDL renders the CREATE INDEX statement (schema-qualified, quoted identifiers).
func (c Candidate) DDL() string {
	cols := make([]string, len(c.Columns))
	for i, col := range c.Columns {
		cols[i] = quoteIdent(col)
	}
	return fmt.Sprintf("CREATE INDEX ON %s.%s (%s)", quoteIdent(c.Schema), quoteIdent(c.Table), strings.Join(cols, ", "))
}

// key uniquely identifies a candidate for dedup.
func (c Candidate) key() string {
	return c.Schema + "." + c.Table + "(" + strings.Join(c.Columns, ",") + ")"
}

// Recommendation is a candidate the planner confirmed it would use.
type Recommendation struct {
	Candidate  Candidate `json:"-"`
	IndexDDL   string    `json:"index"`
	Schema     string    `json:"schema"`
	Table      string    `json:"table"`
	Columns    []string  `json:"columns"`
	QueryID    int64     `json:"queryid"`
	QueryText  string    `json:"query"` // scrubbed, normalized ($1)
	Calls      int64     `json:"calls"`
	SharePct   float64   `json:"db_time_pct"`
	CostBefore float64   `json:"cost_before"`
	CostAfter  float64   `json:"cost_after"`
	// EstimatedBytes is hypopg's size estimate for the index that would be built.
	EstimatedBytes int64 `json:"estimated_bytes"`
	// Confidence is capped: a recommendation is a planner ESTIMATE over cost, not a
	// measurement, so it never asserts. Caveats carry the load-bearing "but…".
	Confidence float64  `json:"confidence"`
	Caveats    []string `json:"caveats,omitempty"`
}

// ImprovementPct is the estimated total-cost reduction, 0..100.
func (r Recommendation) ImprovementPct() float64 {
	if r.CostBefore <= 0 {
		return 0
	}
	return (r.CostBefore - r.CostAfter) / r.CostBefore * 100
}

// ---- pure plan analysis (unit-tested without a database) ----

// planNode is the subset of an EXPLAIN (FORMAT JSON) node we read.
type planNode struct {
	NodeType     string     `json:"Node Type"`
	RelationName string     `json:"Relation Name"`
	Schema       string     `json:"Schema"`
	Alias        string     `json:"Alias"`
	Filter       string     `json:"Filter"`
	IndexName    string     `json:"Index Name"`
	TotalCost    float64    `json:"Total Cost"`
	Plans        []planNode `json:"Plans"`
}

// explainRoot is the top-level array element of EXPLAIN (FORMAT JSON).
type explainRoot struct {
	Plan planNode `json:"Plan"`
}

// parsePlan decodes EXPLAIN (FORMAT JSON) output into the root plan node.
func parsePlan(js []byte) (planNode, error) {
	var roots []explainRoot
	if err := json.Unmarshal(js, &roots); err != nil {
		return planNode{}, err
	}
	if len(roots) == 0 {
		return planNode{}, fmt.Errorf("empty EXPLAIN output")
	}
	return roots[0].Plan, nil
}

// planCost returns the root node's estimated total cost.
func planCost(root planNode) float64 { return root.TotalCost }

// walk visits every node depth-first.
func walk(n planNode, fn func(planNode)) {
	fn(n)
	for _, c := range n.Plans {
		walk(c, fn)
	}
}

// systemSchemas are never index-advised.
var systemSchemas = map[string]bool{"pg_catalog": true, "information_schema": true, "pg_toast": true}

var (
	reEqCol    = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*=\s*\$\d+`)
	reRangeCol = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*(?:<|>|<=|>=)\s*\$\d+`)
)

// candidatesFromPlan extracts index candidates from every Seq Scan whose filter
// references parameter placeholders. Equality columns lead (they belong at the
// front of a composite index), then range columns. Extraction is heuristic on
// purpose — hypopg validation is the safety net, so a useless candidate simply
// never gets recommended. Bad/complex filters yield nothing rather than junk DDL.
func candidatesFromPlan(root planNode) []Candidate {
	var out []Candidate
	seen := map[string]bool{}
	walk(root, func(n planNode) {
		if n.NodeType != "Seq Scan" || n.RelationName == "" || n.Filter == "" {
			return
		}
		// PostgreSQL emits Schema only for a VERBOSE plan. Without it there is
		// no safe way to distinguish same-named relations resolved by search_path.
		schema := n.Schema
		if schema == "" {
			return
		}
		if systemSchemas[schema] {
			return
		}
		cols := filterColumns(n.Filter)
		if len(cols) == 0 {
			return
		}
		c := Candidate{Schema: schema, Table: n.RelationName, Columns: cols}
		if !seen[c.key()] {
			seen[c.key()] = true
			out = append(out, c)
		}
	})
	return out
}

// filterColumns pulls candidate columns from a Seq Scan filter string: equality
// columns (col = $N) first, in first-seen order, then range columns, deduped.
// Only bare column identifiers are taken — a filter over an expression or
// function yields nothing (we won't propose an index we can't express safely).
func filterColumns(filter string) []string {
	var cols []string
	seen := map[string]bool{}
	add := func(c string) {
		c = strings.TrimPrefix(c, "") // identifiers already bare
		if c == "" || seen[c] || isNoiseIdent(c) {
			return
		}
		seen[c] = true
		cols = append(cols, c)
	}
	for _, m := range reEqCol.FindAllStringSubmatch(filter, -1) {
		add(m[1])
	}
	for _, m := range reRangeCol.FindAllStringSubmatch(filter, -1) {
		add(m[1])
	}
	// Cap composite width — beyond a few columns the planner rarely benefits and
	// the candidate stops being reviewable.
	if len(cols) > 4 {
		cols = cols[:4]
	}
	return cols
}

// isNoiseIdent rejects SQL keywords that the regex can catch as "columns".
func isNoiseIdent(s string) bool {
	switch strings.ToLower(s) {
	case "and", "or", "not", "null", "true", "false", "is", "in", "any", "all":
		return true
	}
	return false
}

// usesIndex reports whether any node in the plan uses an index of the given name
// (an Index Scan / Index Only Scan / Bitmap Index Scan on the hypothetical index).
func usesIndex(root planNode, indexName string) bool {
	found := false
	walk(root, func(n planNode) {
		if n.IndexName != "" && n.IndexName == indexName {
			found = true
		}
	})
	return found
}

// quoteIdent double-quotes an identifier only when needed (keeps common DDL
// readable) and escapes embedded quotes.
func quoteIdent(s string) string {
	if reSafeIdent.MatchString(s) {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

var reSafeIdent = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// dedupeRecommendations keeps the highest-share recommendation per candidate
// index — the same index often helps several queries; we attribute it to the
// most impactful one and don't repeat the DDL.
func dedupeRecommendations(recs []Recommendation) []Recommendation {
	best := map[string]Recommendation{}
	for _, r := range recs {
		k := r.Candidate.key()
		if cur, ok := best[k]; !ok || r.SharePct > cur.SharePct {
			best[k] = r
		}
	}
	out := make([]Recommendation, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SharePct != out[j].SharePct {
			return out[i].SharePct > out[j].SharePct
		}
		return out[i].ImprovementPct() > out[j].ImprovementPct()
	})
	return out
}
