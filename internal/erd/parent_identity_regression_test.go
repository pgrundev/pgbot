package erd

import (
	"strings"
	"testing"
)

func TestTableIdentityParentRegression(t *testing.T) {
	s := Schema{Tables: []Table{{Schema: "a.b", Name: "users", Columns: []Column{{Name: "id", Type: "bigint"}}}, {Schema: "a", Name: "b.users", Columns: []Column{{Name: "id", Type: "bigint"}}}}}
	got := RenderASCII(s, false)
	if strings.Contains(got, "a.b.users") {
		t.Fatalf("distinct table coordinates collapse into identical labels: %s", got)
	}
	if !strings.Contains(got, `"a.b".users`) || !strings.Contains(got, `a."b.users"`) {
		t.Fatalf("qualified labels missing: %s", got)
	}
}
