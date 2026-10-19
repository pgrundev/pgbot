package ai

import (
	"strings"
	"testing"
)

func TestBuildPlanPrompt_groundsTheModelOnTheRenderedPlan(t *testing.T) {
	system, user := BuildPlanPrompt("SELECT * FROM orders WHERE status = $1", "Seq Scan on orders\n  reads every row of orders, planner expects about 1.2M rows")
	for _, want := range []string{"SELECT * FROM orders WHERE status = $1", "Seq Scan on orders", "1.2M"} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt lacks %q:\n%s", want, user)
		}
	}
	// The model must treat numbers as estimates and never ask the user to run the query.
	low := strings.ToLower(system)
	for _, want := range []string{"estimate", "not executed", "never"} {
		if !strings.Contains(low, want) {
			t.Errorf("system prompt lacks %q:\n%s", want, system)
		}
	}
	if !strings.Contains(low, "no markdown") {
		t.Errorf("system prompt should demand plain text")
	}
}
