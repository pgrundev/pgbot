package plan

import (
	"strings"
	"testing"
)

// fixtureJoin is EXPLAIN (GENERIC_PLAN, FORMAT JSON) for
//
//	SELECT c.name, count(*) FROM orders o JOIN customers c ON c.id = o.customer_id
//	WHERE o.status = $1 GROUP BY c.name ORDER BY 2 DESC
//
// shaped the way PostgreSQL 16 emits it: a Sort over a HashAggregate over a
// Nested Loop whose inner side is a Seq Scan.
const fixtureJoin = `[
  {
    "Plan": {
      "Node Type": "Sort",
      "Parallel Aware": false,
      "Startup Cost": 25000.10,
      "Total Cost": 25010.20,
      "Plan Rows": 200,
      "Plan Width": 40,
      "Sort Key": ["(count(*)) DESC"],
      "Plans": [
        {
          "Node Type": "Aggregate",
          "Strategy": "Hashed",
          "Partial Mode": "Simple",
          "Parent Relationship": "Outer",
          "Parallel Aware": false,
          "Startup Cost": 24000.00,
          "Total Cost": 24002.00,
          "Plan Rows": 200,
          "Plan Width": 40,
          "Group Key": ["c.name"],
          "Plans": [
            {
              "Node Type": "Nested Loop",
              "Parent Relationship": "Outer",
              "Parallel Aware": false,
              "Join Type": "Inner",
              "Startup Cost": 0.00,
              "Total Cost": 23000.00,
              "Plan Rows": 1500000,
              "Plan Width": 32,
              "Inner Unique": true,
              "Join Filter": "(c.id = o.customer_id)",
              "Plans": [
                {
                  "Node Type": "Index Scan",
                  "Parent Relationship": "Outer",
                  "Parallel Aware": false,
                  "Scan Direction": "Forward",
                  "Index Name": "orders_status_idx",
                  "Relation Name": "orders",
                  "Schema": "public",
                  "Alias": "o",
                  "Startup Cost": 0.43,
                  "Total Cost": 9000.00,
                  "Plan Rows": 1500000,
                  "Plan Width": 8,
                  "Index Cond": "(status = $1)",
                  "Filter": "(amount > '0'::numeric)"
                },
                {
                  "Node Type": "Seq Scan",
                  "Parent Relationship": "Inner",
                  "Parallel Aware": false,
                  "Relation Name": "customers",
                  "Schema": "public",
                  "Alias": "c",
                  "Startup Cost": 0.00,
                  "Total Cost": 12.00,
                  "Plan Rows": 1,
                  "Plan Width": 36
                }
              ]
            }
          ]
        }
      ]
    }
  }
]`

func mustParse(t *testing.T, js string) *Node {
	t.Helper()
	n, err := Parse([]byte(js))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return n
}

func TestParse_readsTheTree(t *testing.T) {
	root := mustParse(t, fixtureJoin)
	if root.NodeType != "Sort" {
		t.Fatalf("root = %q, want Sort", root.NodeType)
	}
	if got := root.SortKey; len(got) != 1 || got[0] != "(count(*)) DESC" {
		t.Errorf("sort key = %v", got)
	}
	agg := root.Plans[0]
	if agg.NodeType != "Aggregate" || agg.Strategy != "Hashed" {
		t.Errorf("agg = %q/%q", agg.NodeType, agg.Strategy)
	}
	join := agg.Plans[0]
	if join.NodeType != "Nested Loop" || join.JoinType != "Inner" || join.PlanRows != 1500000 {
		t.Errorf("join = %+v", join)
	}
	if len(join.Plans) != 2 {
		t.Fatalf("join children = %d, want 2", len(join.Plans))
	}
	outer, inner := join.Plans[0], join.Plans[1]
	if outer.NodeType != "Index Scan" || outer.IndexName != "orders_status_idx" || outer.IndexCond != "(status = $1)" || outer.Filter == "" {
		t.Errorf("outer = %+v", outer)
	}
	if inner.NodeType != "Seq Scan" || inner.RelationName != "customers" || inner.Alias != "c" || inner.TotalCost != 12 {
		t.Errorf("inner = %+v", inner)
	}
}

func TestParse_rejectsEmptyAndGarbage(t *testing.T) {
	for _, js := range []string{``, `[]`, `{}`, `not json`} {
		if _, err := Parse([]byte(js)); err == nil {
			t.Errorf("Parse(%q) = nil error, want failure", js)
		}
	}
}

func TestGloss_speaksPlainEnglishPerNodeType(t *testing.T) {
	cases := []struct {
		node Node
		want []string // substrings that must appear
	}{
		{Node{NodeType: "Seq Scan", RelationName: "orders", PlanRows: 1200000}, []string{"orders", "every row", "1.2M"}},
		{Node{NodeType: "Index Scan", RelationName: "orders", IndexName: "orders_pkey", PlanRows: 1}, []string{"orders_pkey", "orders", "1 row"}},
		{Node{NodeType: "Index Only Scan", RelationName: "t", IndexName: "t_idx", PlanRows: 50}, []string{"t_idx", "without", "table"}},
		{Node{NodeType: "Bitmap Heap Scan", RelationName: "t", PlanRows: 500}, []string{"t", "bitmap"}},
		{Node{NodeType: "Bitmap Index Scan", IndexName: "t_idx", PlanRows: 500}, []string{"t_idx", "bitmap"}},
		{Node{NodeType: "Nested Loop", JoinType: "Inner", PlanRows: 10}, []string{"inner join", "each", "outer"}},
		{Node{NodeType: "Hash Join", JoinType: "Left", PlanRows: 10}, []string{"left join", "hash"}},
		{Node{NodeType: "Merge Join", JoinType: "Inner", PlanRows: 10}, []string{"inner join", "sorted"}},
		{Node{NodeType: "Hash", PlanRows: 10}, []string{"hash table"}},
		{Node{NodeType: "Sort", SortKey: []string{"created_at DESC"}, PlanRows: 10}, []string{"sort", "created_at DESC"}},
		{Node{NodeType: "Incremental Sort", SortKey: []string{"a", "b"}, PlanRows: 10}, []string{"sort", "already"}},
		{Node{NodeType: "Aggregate", Strategy: "Hashed", GroupKey: []string{"c.name"}, PlanRows: 10}, []string{"group", "c.name", "hash"}},
		{Node{NodeType: "Aggregate", Strategy: "Sorted", GroupKey: []string{"x"}, PlanRows: 10}, []string{"group", "sorted"}},
		{Node{NodeType: "Aggregate", Strategy: "Plain", PlanRows: 1}, []string{"single"}},
		{Node{NodeType: "Limit", PlanRows: 10}, []string{"stop", "10 rows"}},
		{Node{NodeType: "Gather", WorkersPlanned: 2, PlanRows: 10}, []string{"2 parallel workers"}},
		{Node{NodeType: "Gather Merge", WorkersPlanned: 3, PlanRows: 10}, []string{"3 parallel workers", "order"}},
		{Node{NodeType: "Materialize", PlanRows: 10}, []string{"memory", "re-read"}},
		{Node{NodeType: "Memoize", PlanRows: 10}, []string{"cache"}},
		{Node{NodeType: "Append", PlanRows: 10}, []string{"concatenat"}},
		{Node{NodeType: "Unique", PlanRows: 10}, []string{"duplicate"}},
		{Node{NodeType: "CTE Scan", Alias: "recent", PlanRows: 10}, []string{"recent", "WITH"}},
		{Node{NodeType: "Subquery Scan", Alias: "sub", PlanRows: 10}, []string{"subquery"}},
		{Node{NodeType: "Function Scan", Alias: "generate_series", PlanRows: 10}, []string{"function", "generate_series"}},
		{Node{NodeType: "Values Scan", PlanRows: 3}, []string{"VALUES"}},
		{Node{NodeType: "WindowAgg", PlanRows: 3}, []string{"window function"}},
		{Node{NodeType: "Result", PlanRows: 1}, []string{"constant"}},
		{Node{NodeType: "Frobnicate Scan", PlanRows: 1}, []string{"Frobnicate Scan"}}, // unknown: name it, don't crash
	}
	for _, c := range cases {
		got := strings.ToLower(c.node.Gloss())
		for _, w := range c.want {
			if !strings.Contains(got, strings.ToLower(w)) {
				t.Errorf("%s gloss %q lacks %q", c.node.NodeType, c.node.Gloss(), w)
			}
		}
	}
}

func TestHumanRows(t *testing.T) {
	cases := map[float64]string{0: "0", 1: "1", 999: "999", 1000: "1.0K", 12345: "12.3K", 1200000: "1.2M", 3400000000: "3.4B"}
	for in, want := range cases {
		if got := HumanRows(in); got != want {
			t.Errorf("HumanRows(%v) = %q, want %q", in, got, want)
		}
	}
}

func findObs(obs []Observation, sub string) *Observation {
	for i := range obs {
		if strings.Contains(strings.ToLower(obs[i].Text), strings.ToLower(sub)) {
			return &obs[i]
		}
	}
	return nil
}

func TestObserve_nestedLoopRepeatingInnerScan(t *testing.T) {
	obs := Observe(mustParse(t, fixtureJoin))
	o := findObs(obs, "nested loop")
	if o == nil {
		t.Fatalf("no nested-loop observation in %+v", obs)
	}
	if o.Level != LevelWarn {
		t.Errorf("level = %q, want warn", o.Level)
	}
	for _, w := range []string{"customers", "1.5M"} {
		if !strings.Contains(o.Text, w) {
			t.Errorf("observation %q lacks %q", o.Text, w)
		}
	}
}

func TestObserve_indexScanWithPostFilter(t *testing.T) {
	obs := Observe(mustParse(t, fixtureJoin))
	o := findObs(obs, "orders_status_idx")
	if o == nil {
		t.Fatalf("no post-filter observation in %+v", obs)
	}
	if o.Level != LevelInfo || !strings.Contains(o.Text, "amount > '0'::numeric") {
		t.Errorf("observation = %+v", *o)
	}
}

func TestObserve_largeSeqScanWarns_smallOneDoesNot(t *testing.T) {
	big := &Node{NodeType: "Seq Scan", RelationName: "events", PlanRows: 2500000, Filter: "(kind = $1)"}
	obs := Observe(big)
	o := findObs(obs, "events")
	if o == nil || o.Level != LevelWarn || !strings.Contains(o.Text, "2.5M") || !strings.Contains(o.Text, "kind = $1") {
		t.Fatalf("big seq scan: %+v", obs)
	}
	small := &Node{NodeType: "Seq Scan", RelationName: "countries", PlanRows: 240}
	if o := findObs(Observe(small), "countries"); o != nil && o.Level == LevelWarn {
		t.Errorf("small seq scan should not warn: %+v", *o)
	}
}

func TestObserve_sortOfManyRows(t *testing.T) {
	root := &Node{NodeType: "Sort", SortKey: []string{"created_at DESC"}, PlanRows: 400000,
		Plans: []Node{{NodeType: "Seq Scan", RelationName: "events", PlanRows: 400000}}}
	o := findObs(Observe(root), "sort")
	if o == nil || o.Level != LevelInfo || !strings.Contains(o.Text, "created_at DESC") || !strings.Contains(o.Text, "400.0K") {
		t.Fatalf("sort observation: %+v", Observe(root))
	}
}

func TestObserve_quietPlanHasNoObservations(t *testing.T) {
	root := &Node{NodeType: "Index Scan", RelationName: "users", IndexName: "users_pkey", IndexCond: "(id = $1)", PlanRows: 1}
	if obs := Observe(root); len(obs) != 0 {
		t.Errorf("want no observations, got %+v", obs)
	}
}

func TestPrepare_buildsPlanOnlyExplain(t *testing.T) {
	stmt, err := Prepare("SELECT * FROM t WHERE id = $1;", 160000)
	if err != nil {
		t.Fatal(err)
	}
	if stmt != "EXPLAIN (GENERIC_PLAN, FORMAT JSON) SELECT * FROM t WHERE id = $1" {
		t.Errorf("pg16 stmt = %q", stmt)
	}
	stmt, err = Prepare("select 1", 150000)
	if err != nil {
		t.Fatal(err)
	}
	if stmt != "EXPLAIN (FORMAT JSON) select 1" {
		t.Errorf("pg15 stmt = %q", stmt)
	}
}

func TestPrepare_refusesAnythingButOneSelect(t *testing.T) {
	for _, q := range []string{
		"",
		"UPDATE t SET a = 1",
		"DELETE FROM t",
		"WITH x AS (DELETE FROM t RETURNING *) SELECT * FROM x",
		"SELECT 1; DROP TABLE t",
		"SELECT 1; SELECT 2",
		"EXPLAIN SELECT 1",
		"selection",
	} {
		if _, err := Prepare(q, 160000); err == nil {
			t.Errorf("Prepare(%q) accepted, want refusal", q)
		}
	}
}

func TestHasPlaceholders(t *testing.T) {
	yes := []string{"SELECT * FROM t WHERE id = $1", "select $2, $10 from t", "SELECT * FROM t WHERE a=$1AND b=2"}
	no := []string{"SELECT 1", "SELECT '$1' FROM t", "SELECT '$' || 'x'", "SELECT * FROM t WHERE price > 5$"}
	for _, q := range yes {
		if !HasPlaceholders(q) {
			t.Errorf("HasPlaceholders(%q) = false", q)
		}
	}
	for _, q := range no {
		if HasPlaceholders(q) {
			t.Errorf("HasPlaceholders(%q) = true", q)
		}
	}
}
