// Package plan turns EXPLAIN (FORMAT JSON) output into something a developer who
// is not a DBA can read: a parsed node tree, a one-line plain-English gloss per
// node, and a short list of deterministic observations about the plan's shape.
//
// Everything here works on the planner's ESTIMATES. pgbot only ever runs the
// plan-only form of EXPLAIN — the inspected query is never executed — so no
// number in this package is a measurement, and callers must say so.
package plan

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pgrundev/pgbot/internal/advisor"
)

// Node is one EXPLAIN (FORMAT JSON) plan node — the fields we read, with the
// JSON names PostgreSQL emits.
type Node struct {
	NodeType           string   `json:"Node Type"`
	Strategy           string   `json:"Strategy"`            // Aggregate: Plain | Sorted | Hashed | Mixed
	ParentRelationship string   `json:"Parent Relationship"` // Outer | Inner | Member | InitPlan | SubPlan
	JoinType           string   `json:"Join Type"`           // Inner | Left | Right | Full | Semi | Anti
	RelationName       string   `json:"Relation Name"`
	Schema             string   `json:"Schema"`
	Alias              string   `json:"Alias"`
	IndexName          string   `json:"Index Name"`
	ScanDirection      string   `json:"Scan Direction"`
	CTEName            string   `json:"CTE Name"`
	SubplanName        string   `json:"Subplan Name"`
	Filter             string   `json:"Filter"`
	IndexCond          string   `json:"Index Cond"`
	RecheckCond        string   `json:"Recheck Cond"`
	HashCond           string   `json:"Hash Cond"`
	MergeCond          string   `json:"Merge Cond"`
	JoinFilter         string   `json:"Join Filter"`
	SortKey            []string `json:"Sort Key"`
	PresortedKey       []string `json:"Presorted Key"`
	GroupKey           []string `json:"Group Key"`
	StartupCost        float64  `json:"Startup Cost"`
	TotalCost          float64  `json:"Total Cost"`
	PlanRows           float64  `json:"Plan Rows"`
	PlanWidth          int      `json:"Plan Width"`
	ParallelAware      bool     `json:"Parallel Aware"`
	WorkersPlanned     int      `json:"Workers Planned"`
	Plans              []Node   `json:"Plans"`
}

type explainRoot struct {
	Plan Node `json:"Plan"`
}

// Parse decodes EXPLAIN (FORMAT JSON) output (a one-element array) into the
// root node.
func Parse(js []byte) (*Node, error) {
	var roots []explainRoot
	if err := json.Unmarshal(js, &roots); err != nil {
		return nil, fmt.Errorf("plan JSON: %w", err)
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("empty EXPLAIN output")
	}
	if roots[0].Plan.NodeType == "" {
		return nil, fmt.Errorf("EXPLAIN output has no plan node")
	}
	return &roots[0].Plan, nil
}

// Prepare validates a user-supplied query and builds the plan-only EXPLAIN
// statement for it. The same SELECT-only sanitizer guards `advise` and the MCP
// explain_plan tool: a single plain SELECT, no CTE (a WITH can hide a writable
// CTE), no interior semicolon. versionNum is the server's integer version
// (e.g. 160000): from 16 on, GENERIC_PLAN plans a query with $N placeholders
// without values; below that, plain EXPLAIN works for literal queries and errors
// clearly on placeholders.
func Prepare(query string, versionNum int) (string, error) {
	clean := advisor.SanitizeSelect(query)
	if clean == "" {
		return "", ErrNotSelect
	}
	if versionNum >= 160000 {
		return "EXPLAIN (GENERIC_PLAN, FORMAT JSON) " + clean, nil
	}
	return "EXPLAIN (FORMAT JSON) " + clean, nil
}

// HumanRows formats a planner row estimate: 999 → "999", 12345 → "12.3K",
// 1200000 → "1.2M".
func HumanRows(f float64) string {
	switch {
	case f >= 1e9:
		return fmt.Sprintf("%.1fB", f/1e9)
	case f >= 1e6:
		return fmt.Sprintf("%.1fM", f/1e6)
	case f >= 1e3:
		return fmt.Sprintf("%.1fK", f/1e3)
	default:
		return fmt.Sprintf("%.0f", f)
	}
}

// rows is "about N rows" with the singular handled.
func rows(f float64) string {
	if f == 1 {
		return "1 row"
	}
	return "about " + HumanRows(f) + " rows"
}

// target names what a scan reads: "orders" or "orders (as o)".
func (n *Node) target() string {
	name := n.RelationName
	if name == "" {
		name = n.Alias
	}
	if n.Alias != "" && n.Alias != n.RelationName && n.RelationName != "" {
		return name + " (as " + n.Alias + ")"
	}
	return name
}

// Gloss is the node in one plain-English sentence with its row estimate.
func (n *Node) Gloss() string {
	r := rows(n.PlanRows)
	jt := strings.ToLower(n.JoinType)
	if jt == "" {
		jt = "inner"
	}
	switch n.NodeType {
	case "Seq Scan":
		return fmt.Sprintf("reads every row of %s, planner expects %s", n.target(), r)
	case "Index Scan":
		return fmt.Sprintf("walks index %s to find rows in %s, then fetches each row from the table; expects %s", n.IndexName, n.target(), r)
	case "Index Only Scan":
		return fmt.Sprintf("answers from index %s alone, without visiting the table %s; expects %s", n.IndexName, n.target(), r)
	case "Bitmap Heap Scan":
		return fmt.Sprintf("fetches the pages of %s marked in a bitmap by the index scan below; expects %s", n.target(), r)
	case "Bitmap Index Scan":
		return fmt.Sprintf("scans index %s and builds a bitmap of matching pages; expects %s", n.IndexName, r)
	case "BitmapAnd", "BitmapOr":
		return fmt.Sprintf("combines the bitmaps below (%s)", strings.TrimPrefix(n.NodeType, "Bitmap"))
	case "Tid Scan":
		return fmt.Sprintf("fetches rows of %s by physical location; expects %s", n.target(), r)
	case "Nested Loop":
		return fmt.Sprintf("%s join: for each row from the outer side, looks up matches on the inner side; expects %s", jt, r)
	case "Hash Join":
		return fmt.Sprintf("%s join: builds a hash table from one side and probes it with the other; expects %s", jt, r)
	case "Merge Join":
		return fmt.Sprintf("%s join: walks two sorted inputs in step; expects %s", jt, r)
	case "Hash":
		return fmt.Sprintf("builds an in-memory hash table from %s", r)
	case "Sort":
		return fmt.Sprintf("sorts %s by %s", r, strings.Join(n.SortKey, ", "))
	case "Incremental Sort":
		return fmt.Sprintf("sorts %s by %s, reusing input already ordered by %s", r, strings.Join(n.SortKey, ", "), strings.Join(n.PresortedKey, ", "))
	case "Aggregate":
		switch n.Strategy {
		case "Hashed":
			return fmt.Sprintf("groups by %s using a hash table; expects %s", strings.Join(n.GroupKey, ", "), r)
		case "Sorted":
			return fmt.Sprintf("groups by %s over sorted input; expects %s", strings.Join(n.GroupKey, ", "), r)
		case "Mixed":
			return fmt.Sprintf("groups by %s, hash and sorted combined; expects %s", strings.Join(n.GroupKey, ", "), r)
		default:
			return "computes a single aggregate result over all input rows"
		}
	case "Group":
		return fmt.Sprintf("groups sorted input by %s; expects %s", strings.Join(n.GroupKey, ", "), r)
	case "WindowAgg":
		return fmt.Sprintf("evaluates window functions over %s", r)
	case "Limit":
		return fmt.Sprintf("stops after %s", r)
	case "Unique":
		return fmt.Sprintf("drops adjacent duplicate rows; expects %s", r)
	case "Gather":
		return fmt.Sprintf("collects rows from %d parallel workers; expects %s", n.WorkersPlanned, r)
	case "Gather Merge":
		return fmt.Sprintf("collects rows from %d parallel workers, preserving sort order; expects %s", n.WorkersPlanned, r)
	case "Materialize":
		return fmt.Sprintf("holds %s in memory so they can be re-read", r)
	case "Memoize":
		return fmt.Sprintf("caches inner-side lookups by key so repeated keys are not rescanned; expects %s", r)
	case "Append":
		return fmt.Sprintf("concatenates the results below (UNION ALL or partitions); expects %s", r)
	case "Merge Append":
		return fmt.Sprintf("concatenates the sorted results below, preserving order; expects %s", r)
	case "Recursive Union":
		return fmt.Sprintf("iterates a recursive WITH query; expects %s", r)
	case "WorkTable Scan":
		return fmt.Sprintf("reads the previous iteration of recursive query %s; expects %s", n.CTEName, r)
	case "CTE Scan":
		return fmt.Sprintf("reads the WITH query %s; expects %s", firstNonEmpty(n.CTEName, n.Alias), r)
	case "Subquery Scan":
		return fmt.Sprintf("reads the subquery %s; expects %s", n.Alias, r)
	case "Function Scan":
		return fmt.Sprintf("calls the function %s and reads its result; expects %s", n.Alias, r)
	case "Values Scan":
		return fmt.Sprintf("reads a literal VALUES list; expects %s", r)
	case "Result":
		return "returns a constant or a single computed row"
	case "SetOp":
		return fmt.Sprintf("applies a set operation (INTERSECT/EXCEPT); expects %s", r)
	case "ProjectSet":
		return fmt.Sprintf("expands set-returning functions in the select list; expects %s", r)
	case "LockRows":
		return fmt.Sprintf("locks the selected rows (FOR UPDATE/SHARE); expects %s", r)
	case "Foreign Scan":
		return fmt.Sprintf("reads %s from a foreign server; expects %s", n.target(), r)
	default:
		return fmt.Sprintf("%s; expects %s", n.NodeType, r)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// HasPlaceholders reports whether the query uses $N parameter placeholders
// outside string literals — the case where a GENERIC_PLAN's "parameter values
// unknown" caveat applies.
func HasPlaceholders(q string) bool {
	inLiteral := false
	for i := 0; i < len(q); i++ {
		c := q[i]
		if c == '\'' {
			inLiteral = !inLiteral
			continue
		}
		if inLiteral || c != '$' {
			continue
		}
		if i+1 < len(q) && q[i+1] >= '0' && q[i+1] <= '9' {
			return true
		}
	}
	return false
}
