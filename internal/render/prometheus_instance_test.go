package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/model"
)

// Two members reporting the same database must not collapse into duplicate
// series (the textfile collector rejects those); without --all-instances the
// label set is exactly what it always was.
func TestPrometheusAll_instanceLabels(t *testing.T) {
	mk := func(inst, role string) *model.Context {
		c := &model.Context{}
		c.Server.Database, c.Server.Instance, c.Server.InstanceRole = "app", inst, role
		c.Findings = []model.Finding{{ID: "table_bloat", Severity: "warn", Object: "public.orders"}}
		return c
	}
	var buf bytes.Buffer
	if err := PrometheusAll(&buf, []*model.Context{mk("w", "writer"), mk("r1", "reader")}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`pgbot_finding{database="app",instance="w",role="writer",id="table_bloat"`,
		`pgbot_finding{database="app",instance="r1",role="reader",id="table_bloat"`,
		`pgbot_findings_total{database="app",instance="w",role="writer",severity="warn"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	buf.Reset()
	if err := PrometheusAll(&buf, []*model.Context{mk("", "")}); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); strings.Contains(out, "instance=") || !strings.Contains(out, `pgbot_finding{database="app",id="table_bloat"`) {
		t.Errorf("single-instance output changed:\n%s", out)
	}
}
