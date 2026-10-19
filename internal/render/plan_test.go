package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/plan"
)

func planFixture() *plan.Node {
	return &plan.Node{
		NodeType: "Hash Join", JoinType: "Inner", HashCond: "(o.customer_id = c.id)", StartupCost: 10.5, TotalCost: 2500.75, PlanRows: 120000,
		Plans: []plan.Node{
			{NodeType: "Seq Scan", RelationName: "orders", Alias: "o", Filter: "(status = $1)", TotalCost: 2000, PlanRows: 120000},
			{NodeType: "Hash", TotalCost: 5, PlanRows: 200, Plans: []plan.Node{
				{NodeType: "Index Only Scan", RelationName: "customers", Alias: "c", IndexName: "customers_pkey", TotalCost: 4, PlanRows: 200},
			}},
		},
	}
}

func TestPlan_rendersTreeGlossAndObservations(t *testing.T) {
	var buf bytes.Buffer
	root := planFixture()
	Plan(&buf, PlanInput{
		Query:        "SELECT * FROM orders o JOIN customers c ON c.id = o.customer_id WHERE o.status = $1",
		Root:         root,
		Observations: plan.Observe(root),
		Generic:      true,
		Color:        false,
	})
	out := buf.String()

	// The honesty header: estimates only, nothing was executed.
	if !strings.Contains(out, "planned, not executed") {
		t.Errorf("missing not-executed header:\n%s", out)
	}
	// The query itself is echoed so the output stands alone.
	if !strings.Contains(out, "WHERE o.status = $1") {
		t.Errorf("query not echoed:\n%s", out)
	}
	// Tree: parent before children, children indented deeper than the parent.
	iJoin := strings.Index(out, "Hash Join")
	iSeq := strings.Index(out, "Seq Scan on orders")
	iIdx := strings.Index(out, "Index Only Scan on customers using customers_pkey")
	if iJoin < 0 || iSeq < 0 || iIdx < 0 || !(iJoin < iSeq && iSeq < iIdx) {
		t.Fatalf("tree order wrong (%d %d %d):\n%s", iJoin, iSeq, iIdx, out)
	}
	if indentOf(out, "Seq Scan on orders") <= indentOf(out, "Hash Join") {
		t.Errorf("Seq Scan should be indented deeper than Hash Join:\n%s", out)
	}
	if indentOf(out, "Index Only Scan on customers") <= indentOf(out, "Hash  ") && indentOf(out, "Index Only Scan on customers") <= indentOf(out, "Seq Scan on orders") {
		t.Errorf("Index Only Scan should be indented deeper than Hash:\n%s", out)
	}
	// Estimates on the node line, gloss and conditions beneath it.
	for _, want := range []string{
		"rows≈120.0K", "cost=10.50..2500.75",
		"reads every row of orders (as o)",
		"Filter: (status = $1)",
		"Hash Cond: (o.customer_id = c.id)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	// Observations section with the seq-scan warning.
	if !strings.Contains(out, "What stands out") || !strings.Contains(out, "Seq Scan on orders (as o) reads the whole table") {
		t.Errorf("observations missing:\n%s", out)
	}
	// Generic-plan caveat.
	if !strings.Contains(out, "generic") {
		t.Errorf("generic plan caveat missing:\n%s", out)
	}
}

func TestPlan_quietPlanSaysSo(t *testing.T) {
	var buf bytes.Buffer
	root := &plan.Node{NodeType: "Index Scan", RelationName: "users", IndexName: "users_pkey", IndexCond: "(id = $1)", PlanRows: 1, TotalCost: 8.3}
	Plan(&buf, PlanInput{Query: "SELECT * FROM users WHERE id = $1", Root: root, Observations: nil})
	out := buf.String()
	if !strings.Contains(out, "Nothing stands out") {
		t.Errorf("quiet plan should say nothing stands out:\n%s", out)
	}
	if strings.Contains(out, "generic") {
		t.Errorf("non-generic plan must not carry the generic caveat:\n%s", out)
	}
}

// indentOf returns the column at which marker's line starts printing non-space
// (tree glyphs count as indentation).
func indentOf(out, marker string) int {
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, marker); i >= 0 {
			return i
		}
	}
	return -1
}
