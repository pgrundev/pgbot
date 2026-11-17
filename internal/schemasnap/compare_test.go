package schemasnap

import "testing"

func TestCompareNamesEachColumnAndIgnoresOrder(t *testing.T) {
	a := baseSnap()
	b := clone(a)
	// b: users gains a column, orders.user_id changes type, and the same
	// columns of users are listed in another order.
	b.Columns = append(b.Columns, Column{RelOID: 100, Attnum: 3, Name: "plan", Type: "text"})
	for i := range b.Columns {
		if b.Columns[i].RelOID == 200 && b.Columns[i].Name == "user_id" {
			b.Columns[i].Type = "integer"
		}
		if b.Columns[i].RelOID == 100 && b.Columns[i].Name == "id" {
			b.Columns[i].Attnum = 9
		}
	}
	b.normalize()
	rep := Compare(a, b, "golden", "prod")
	got := map[string]Difference{}
	for _, d := range rep.Differences {
		got[d.Object+" "+d.Field] = d
	}
	if d, ok := got["public.users.plan column"]; !ok || d.A != "" || d.B != "plan text" {
		t.Errorf("missing column not named: %+v", rep.Differences)
	}
	if d, ok := got["public.orders.user_id column_definition"]; !ok || d.A != "user_id bigint" || d.B != "user_id integer" {
		t.Errorf("changed column not named: %+v", rep.Differences)
	}
	if len(rep.Differences) != 2 {
		t.Errorf("want exactly 2 differences (order ignored), got %+v", rep.Differences)
	}
}
