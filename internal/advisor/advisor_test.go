package advisor

import (
	"context"
	"strings"
	"testing"
)

func TestFilterColumns_equalityFirstThenRange(t *testing.T) {
	cols := filterColumns("((customer_id = $1) AND (created_at > $2) AND (status = $3))")
	// equality (customer_id, status) before range (created_at)
	want := []string{"customer_id", "status", "created_at"}
	if strings.Join(cols, ",") != strings.Join(want, ",") {
		t.Errorf("got %v, want %v", cols, want)
	}
}

func TestFilterColumns_ignoresExpressionsAndKeywords(t *testing.T) {
	// A filter with no bare "col = $N" yields nothing (we won't invent DDL).
	if cols := filterColumns("(lower(email) = $1)"); len(cols) != 0 {
		t.Errorf("expression filter should yield no columns, got %v", cols)
	}
}

func TestCandidatesFromPlan_seqScanOnly(t *testing.T) {
	js := []byte(`[{"Plan":{
		"Node Type":"Aggregate","Total Cost":4040.5,"Plans":[
			{"Node Type":"Seq Scan","Relation Name":"orders","Schema":"public",
			 "Filter":"((customer_id = $1) AND (status = $2))","Total Cost":4000.0},
			{"Node Type":"Seq Scan","Relation Name":"pg_class","Schema":"pg_catalog",
			 "Filter":"(relname = $1)","Total Cost":10.0}
		]}}]`)
	root, err := parsePlan(js)
	if err != nil {
		t.Fatal(err)
	}
	if got := planCost(root); got != 4040.5 {
		t.Errorf("planCost = %v, want 4040.5", got)
	}
	cands := candidatesFromPlan(root)
	if len(cands) != 1 { // pg_catalog scan excluded
		t.Fatalf("expected 1 candidate (system schema excluded), got %d: %+v", len(cands), cands)
	}
	if cands[0].DDL() != "CREATE INDEX ON public.orders (customer_id, status)" {
		t.Errorf("wrong DDL: %s", cands[0].DDL())
	}
}

func TestCandidatesFromPlan_requiresResolvedSchema(t *testing.T) {
	t.Run("schema-less plan fails closed", func(t *testing.T) {
		root, err := parsePlan([]byte(`[{
			"Plan":{"Node Type":"Seq Scan","Relation Name":"orders",
			"Filter":"(customer_id = $1)","Total Cost":4040.0}
		}]`))
		if err != nil {
			t.Fatal(err)
		}
		if got := candidatesFromPlan(root); len(got) != 0 {
			t.Fatalf("schema-less plan must not invent a target schema, got %+v", got)
		}
	})

	t.Run("verbose qualified filter retains non-public schema", func(t *testing.T) {
		root, err := parsePlan([]byte(`[{
			"Plan":{"Node Type":"Seq Scan","Relation Name":"orders","Schema":"tenant_data",
			"Filter":"(orders.customer_id = $1)","Total Cost":4040.0}
		}]`))
		if err != nil {
			t.Fatal(err)
		}
		got := candidatesFromPlan(root)
		if len(got) != 1 || got[0].DDL() != "CREATE INDEX ON tenant_data.orders (customer_id)" {
			t.Fatalf("wrong candidate from verbose plan: %+v", got)
		}
	})
}

func TestUsesIndex(t *testing.T) {
	js := []byte(`[{"Plan":{"Node Type":"Index Scan","Index Name":"<13337>btree_orders_customer_id","Total Cost":52.0}}]`)
	root, _ := parsePlan(js)
	if !usesIndex(root, "<13337>btree_orders_customer_id") {
		t.Error("should detect the hypo index by name")
	}
	if usesIndex(root, "some_other_index") {
		t.Error("must not match a different index name")
	}
}

func TestQuoteIdent(t *testing.T) {
	if quoteIdent("customer_id") != "customer_id" {
		t.Error("safe identifier should not be quoted")
	}
	if quoteIdent("Weird Col") != `"Weird Col"` {
		t.Errorf("unsafe identifier should be quoted, got %s", quoteIdent("Weird Col"))
	}
}

func TestSanitizeQuery(t *testing.T) {
	cases := map[string]string{
		"SELECT * FROM t WHERE a = $1":    "SELECT * FROM t WHERE a = $1",
		"select 1;":                       "select 1",
		"  SELECT x FROM y ; ":            "SELECT x FROM y",
		"WITH w AS (...) SELECT * FROM w": "", // WITH refused (writable-CTE risk)
		"UPDATE t SET x = 1":              "", // DML refused
		"SELECT 1; DROP TABLE t":          "", // second statement refused
	}
	for in, want := range cases {
		if got := sanitizeQuery(in); got != want {
			t.Errorf("sanitizeQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

// mockPlanner simulates hypopg: the candidate index makes the plan cheap and used.
type mockPlanner struct {
	created  int
	resetN   int
	hypoName string
}

func (m *mockPlanner) GenericPlan(_ context.Context, _ string) ([]byte, error) {
	if m.hypoName == "" { // no hypo index active → expensive Seq Scan
		return []byte(`[{"Plan":{"Node Type":"Seq Scan","Relation Name":"orders","Schema":"public","Filter":"(customer_id = $1)","Total Cost":4040.0}}]`), nil
	}
	// hypo index active → cheap Index Scan using it
	return []byte(`[{"Plan":{"Node Type":"Index Scan","Index Name":"` + m.hypoName + `","Relation Name":"orders","Total Cost":52.0}}]`), nil
}
func (m *mockPlanner) CreateHypoIndex(_ context.Context, _ string) (string, int64, error) {
	m.created++
	m.hypoName = "<13337>btree_orders_customer_id"
	return m.hypoName, 1 << 20, nil
}
func (m *mockPlanner) ResetHypo(_ context.Context) error {
	m.resetN++
	m.hypoName = ""
	return nil
}

func TestAdvise_validatesAndRecommends(t *testing.T) {
	m := &mockPlanner{}
	recs, st := Advise(context.Background(), m, []QueryInput{
		{QueryID: 1, Text: "SELECT * FROM orders WHERE customer_id = $1", Scrubbed: "SELECT * FROM orders WHERE customer_id = $1", Calls: 1000, SharePct: 45},
	}, Options{MinImprovement: 0.5})

	if len(recs) != 1 {
		t.Fatalf("expected 1 recommendation, got %d", len(recs))
	}
	r := recs[0]
	if r.CostBefore != 4040 || r.CostAfter != 52 {
		t.Errorf("cost delta wrong: %v → %v", r.CostBefore, r.CostAfter)
	}
	if r.ImprovementPct() < 98 {
		t.Errorf("improvement should be ~98.7%%, got %.1f", r.ImprovementPct())
	}
	if st.CandidatesTested != 1 || st.QueriesPlanned != 1 {
		t.Errorf("stats wrong: %+v", st)
	}
	if m.resetN < 1 {
		t.Error("hypopg must be reset after validation")
	}
}

func TestAdvise_rejectsWhenIndexUnused(t *testing.T) {
	// A planner whose plan never uses the hypo index → no recommendation.
	m := &staticPlanner{plan: `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"orders","Schema":"public","Filter":"(customer_id = $1)","Total Cost":4040.0}}]`}
	recs, _ := Advise(context.Background(), m, []QueryInput{
		{QueryID: 1, Text: "SELECT * FROM orders WHERE customer_id = $1"},
	}, Options{})
	if len(recs) != 0 {
		t.Errorf("must not recommend an index the planner ignores, got %v", recs)
	}
}

type staticPlanner struct{ plan string }

func (s *staticPlanner) GenericPlan(context.Context, string) ([]byte, error) {
	return []byte(s.plan), nil
}
func (s *staticPlanner) CreateHypoIndex(context.Context, string) (string, int64, error) {
	return "<1>btree_orders", 0, nil
}
func (s *staticPlanner) ResetHypo(context.Context) error { return nil }
