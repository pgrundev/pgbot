package report

import (
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/model"
)

func TestRenderFindingsOrdersEverySeverityAcrossInputPermutations(t *testing.T) {
	critical := []model.Finding{
		{ID: "critical_live", Severity: model.SeverityCritical, Title: "critical live"},
		{ID: "critical_suppressed", Severity: model.SeverityCritical, Title: "critical suppressed", Suppressed: true},
	}
	warn := []model.Finding{
		{ID: "warn_live", Severity: model.SeverityWarn, Title: "warn live"},
		{ID: "warn_suppressed", Severity: model.SeverityWarn, Title: "warn suppressed", Suppressed: true},
	}
	info := []model.Finding{
		{ID: "info_live", Severity: model.SeverityInfo, Title: "info live"},
		{ID: "info_suppressed", Severity: model.SeverityInfo, Title: "info suppressed", Suppressed: true},
	}

	tests := []struct {
		name     string
		findings []model.Finding
	}{
		{"warning before critical", append(append(append([]model.Finding{}, warn...), critical...), info...)},
		{"info before warning before critical", append(append(append([]model.Finding{}, info...), warn...), critical...)},
		{"interleaved", []model.Finding{info[0], warn[0], critical[0], info[1], warn[1], critical[1]}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := Render(&model.Context{Findings: tt.findings}, 0, "test")
			assertFindingGroupBefore(t, out, critical, warn)
			assertFindingGroupBefore(t, out, warn, info)
		})
	}
}

func assertFindingGroupBefore(t *testing.T, html string, first, second []model.Finding) {
	t.Helper()
	for _, a := range first {
		ai := strings.Index(html, a.Title)
		if ai < 0 {
			t.Fatalf("rendered report is missing %q", a.Title)
		}
		for _, b := range second {
			bi := strings.Index(html, b.Title)
			if bi < 0 {
				t.Fatalf("rendered report is missing %q", b.Title)
			}
			if ai > bi {
				t.Errorf("%q rendered after lower-severity %q", a.Title, b.Title)
			}
		}
	}
}
