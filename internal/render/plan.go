package render

import (
	"fmt"
	"io"
	"strings"

	"github.com/pgrundev/pgbot/internal/plan"
)

// PlanInput is what `pgbot sql explain` renders: the query, its parsed plan,
// the deterministic observations, and whether the plan is a GENERIC_PLAN (PG16+,
// parameter values unknown to the planner).
type PlanInput struct {
	Query        string
	Root         *plan.Node
	Observations []plan.Observation
	Generic      bool
	Color        bool
}

// Plan writes the human reading of an EXPLAIN plan: an honesty header (nothing
// was executed), the query, the plan tree with a plain-English gloss per node,
// what stands out, and the estimate caveat. Everything is derived from the
// planner's estimates; the text says so where it matters.
func Plan(w io.Writer, in PlanInput) {
	st := styler{on: in.Color}
	fmt.Fprintln(w, st.head("Query plan")+st.dim("  ·  estimates only — the query was planned, not executed"))
	for _, line := range strings.Split(strings.TrimSpace(in.Query), "\n") {
		fmt.Fprintln(w, st.dim("  "+line))
	}
	fmt.Fprintln(w)
	if in.Root != nil {
		writePlanNode(w, st, in.Root, "", true, true)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, st.head("What stands out"))
	if len(in.Observations) == 0 {
		fmt.Fprintln(w, "  "+st.good("Nothing stands out:")+" the planner expects this query to be cheap.")
	}
	for _, o := range in.Observations {
		mark := st.info("·")
		if o.Level == plan.LevelWarn {
			mark = st.warn("!")
		}
		fmt.Fprintln(w, "  "+mark+" "+o.Text)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, st.dim("Costs and row counts are the planner's estimates, not measurements."))
	if in.Generic {
		fmt.Fprintln(w, st.dim("Planned as a generic plan: parameter values ($1, …) were unknown, so the estimates assume typical values."))
	}
}

// writePlanNode draws one node and its subtree. prefix is the indentation the
// parent established; last says whether this is the parent's final child, which
// decides the branch glyph and whether a vertical rule continues below.
func writePlanNode(w io.Writer, st styler, n *plan.Node, prefix string, last, root bool) {
	connector, childPrefix := "", "  "
	if !root {
		if last {
			connector, childPrefix = prefix+"└─ ", prefix+"     "
		} else {
			connector, childPrefix = prefix+"├─ ", prefix+"│    "
		}
	}
	fmt.Fprintln(w, connector+st.head(planLabel(n))+st.dim(fmt.Sprintf("  cost=%.2f..%.2f  rows≈%s", n.StartupCost, n.TotalCost, plan.HumanRows(n.PlanRows))))
	fmt.Fprintln(w, childPrefix+"  "+n.Gloss())
	for _, c := range []struct{ k, v string }{
		{"Index Cond", n.IndexCond}, {"Recheck Cond", n.RecheckCond}, {"Hash Cond", n.HashCond},
		{"Merge Cond", n.MergeCond}, {"Join Filter", n.JoinFilter}, {"Filter", n.Filter},
	} {
		if c.v != "" {
			fmt.Fprintln(w, childPrefix+"  "+st.dim(c.k+": "+c.v))
		}
	}
	for i := range n.Plans {
		writePlanNode(w, st, &n.Plans[i], childPrefix, i == len(n.Plans)-1, false)
	}
}

// planLabel is the node's one-line name: "Seq Scan on orders (as o)",
// "Index Only Scan on customers using customers_pkey (as c)", "Hash Join (inner)".
func planLabel(n *plan.Node) string {
	b := strings.Builder{}
	b.WriteString(n.NodeType)
	if n.Strategy != "" && n.NodeType == "Aggregate" {
		b.WriteString(" (" + strings.ToLower(n.Strategy) + ")")
	}
	if n.JoinType != "" {
		b.WriteString(" (" + strings.ToLower(n.JoinType) + ")")
	}
	if n.RelationName != "" {
		b.WriteString(" on " + n.RelationName)
	}
	if n.IndexName != "" {
		b.WriteString(" using " + n.IndexName)
	}
	if n.Alias != "" && n.Alias != n.RelationName {
		if n.RelationName == "" {
			b.WriteString(" " + n.Alias)
		} else {
			b.WriteString(" (as " + n.Alias + ")")
		}
	}
	if n.ParallelAware {
		b.WriteString(" [parallel]")
	}
	return b.String()
}
