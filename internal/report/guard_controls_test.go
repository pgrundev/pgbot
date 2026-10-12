package report

import (
	"html"
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/model"
)

func TestReportGuardsPreserveProhibitionsAndEscaping(t *testing.T) {
	verify := `confirm <replica> & "owner"`
	text := `guard <strong>marker</strong> & evidence`
	for _, suppressed := range []bool{false, true} {
		c := &model.Context{Findings: []model.Finding{{
			ID: "synthetic_guard", Severity: model.SeverityWarn, Title: "guard fixture",
			Suppressed: suppressed,
			Safety: &model.Safety{BlockingCaveats: []model.SafetyGuard{
				{ID: "precondition", Kind: model.GuardPrecondition, Action: model.ActionDropIndex, Text: text, Verify: &verify},
				{ID: "prohibition", Kind: model.GuardProhibition, Action: model.ActionVacuumFull, Text: "preserve corruption evidence"},
			}},
		}}}
		out := Render(c, 50, "test")
		for _, want := range []string{"Caution before DROP INDEX", "Do not run VACUUM FULL", html.EscapeString(text), html.EscapeString(verify), "preserve corruption evidence"} {
			if !strings.Contains(out, want) {
				t.Errorf("suppressed=%v: missing %q", suppressed, want)
			}
		}
		if strings.Contains(out, text) || strings.Contains(out, verify) {
			t.Fatal("guard content was not HTML-escaped")
		}
		if strings.Count(out, `class="guard"`) != 2 {
			t.Fatal("every blocking caveat must be rendered once")
		}
	}
}

func TestReportWithoutSafetyDoesNotInventGuards(t *testing.T) {
	for _, safety := range []*model.Safety{nil, {}} {
		c := &model.Context{Findings: []model.Finding{{
			ID: "fixture", Severity: model.SeverityInfo, Title: "ordinary finding", Safety: safety,
		}}}
		out := Render(c, 100, "test")
		if strings.Contains(out, `class="guard"`) {
			t.Fatal("unexpected guard for an unguarded finding")
		}
		if !strings.Contains(out, "ordinary finding") {
			t.Fatal("ordinary finding disappeared")
		}
	}
}
