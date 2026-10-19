package plan

import (
	"fmt"
	"strings"
)

// Observation levels. Warn is "the planner expects this to be expensive"; Info
// is "worth knowing, probably fine".
const (
	LevelWarn = "warn"
	LevelInfo = "info"
)

// Observation is one deterministic remark about the plan's shape, derived from
// the planner's estimates only.
type Observation struct {
	Level string
	Text  string
}

// Thresholds on planner row estimates. They are deliberately coarse: the point
// is to flag the shapes that are expensive at scale, not to grade every node.
const (
	bigScanRows  = 10_000 // a sequential scan expected to return this many rows is worth a look
	bigOuterRows = 1_000  // a nested loop repeating its inner side this many times is worth a look
	bigSortRows  = 10_000
)

// Observe walks the tree and returns the observations, warnings first, in the
// order the nodes appear.
func Observe(root *Node) []Observation {
	var warn, info []Observation
	add := func(o Observation) {
		if o.Level == LevelWarn {
			warn = append(warn, o)
		} else {
			info = append(info, o)
		}
	}
	walk(root, func(n *Node) {
		switch n.NodeType {
		case "Seq Scan":
			if n.PlanRows >= bigScanRows {
				text := fmt.Sprintf("Seq Scan on %s reads the whole table; the planner expects about %s rows back.", n.target(), HumanRows(n.PlanRows))
				if n.Filter != "" {
					text += fmt.Sprintf(" Rows are filtered by %s after being read — if that filter is selective, an index on its columns could let the planner skip most of the table.", n.Filter)
				}
				add(Observation{LevelWarn, text})
			}
		case "Nested Loop":
			if len(n.Plans) == 2 {
				outer, inner := &n.Plans[0], &n.Plans[1]
				if outer.PlanRows >= bigOuterRows && isScan(inner) {
					add(Observation{LevelWarn, fmt.Sprintf("Nested Loop repeats a %s on %s once per outer row — about %s times. An index on the join column of %s, or a hash join, would avoid that.",
						inner.NodeType, inner.target(), HumanRows(outer.PlanRows), inner.target())})
				}
			}
		case "Index Scan", "Index Only Scan", "Bitmap Heap Scan":
			if n.Filter != "" {
				idx := n.IndexName
				if idx == "" && len(n.Plans) > 0 {
					idx = n.Plans[0].IndexName
				}
				add(Observation{LevelInfo, fmt.Sprintf("The scan via %s on %s applies %s after the index lookup, so some rows are fetched and then discarded. Adding those columns to the index would skip them.", idx, n.target(), n.Filter)})
			}
		case "Sort":
			if n.PlanRows >= bigSortRows {
				add(Observation{LevelInfo, fmt.Sprintf("Sort of about %s rows by %s. An index ordered the same way would let the planner read rows pre-sorted and skip this step.", HumanRows(n.PlanRows), strings.Join(n.SortKey, ", "))})
			}
		}
	})
	return append(warn, info...)
}

func isScan(n *Node) bool {
	switch n.NodeType {
	case "Seq Scan", "Index Scan", "Index Only Scan", "Bitmap Heap Scan", "Tid Scan", "Foreign Scan":
		return true
	}
	return false
}

func walk(n *Node, fn func(*Node)) {
	fn(n)
	for i := range n.Plans {
		walk(&n.Plans[i], fn)
	}
}
