package schemasnap

import "testing"

func invalidIndexSnap(capturedAt string, valid bool, withIndex bool) *Snapshot {
	s := &Snapshot{Kind: SnapshotKind, CapturedAt: capturedAt, Database: "app",
		Relations: []Relation{{OID: 100, Schema: "public", Name: "orders", Kind: "table", Relfilenode: 100, Bytes: 1 << 30}}}
	if withIndex {
		s.Indexes = []Index{{OID: 200, RelOID: 100, Schema: "public", Name: "orders_status_idx",
			Definition: "CREATE INDEX orders_status_idx ON public.orders USING btree (status)", Valid: valid}}
	}
	return s
}

func findingsWithID(rep DiffReport, id string) []Finding {
	var out []Finding
	for _, f := range rep.Findings {
		if f.ID == id {
			out = append(out, f)
		}
	}
	return out
}

// #153: an index that was already INVALID in the before snapshot is not the
// migration's doing; it must not be critical or make the diff dangerous.
func TestDiff_preexistingInvalidIndexIsNotAttributed(t *testing.T) {
	rep := Diff(invalidIndexSnap("t0", false, true), invalidIndexSnap("t1", false, true), DiffOptions{})
	if rep.Summary.Critical != 0 || rep.Summary.Warning != 0 || rep.Dangerous {
		t.Fatalf("no-op diff with a pre-existing invalid index: summary=%+v dangerous=%v findings=%+v",
			rep.Summary, rep.Dangerous, rep.Findings)
	}
	got := findingsWithID(rep, "index_invalid")
	if len(got) != 1 || got[0].Severity != SevInfo {
		t.Fatalf("pre-existing invalid index should be noted once as info; got %+v", got)
	}
}

// The migration's own invalid index — new, or valid before and invalid after —
// is still critical.
func TestDiff_invalidIndexFromMigrationIsCritical(t *testing.T) {
	cases := map[string]*Snapshot{
		"new index":      invalidIndexSnap("t0", false, false),
		"became invalid": invalidIndexSnap("t0", true, true),
	}
	for name, before := range cases {
		t.Run(name, func(t *testing.T) {
			rep := Diff(before, invalidIndexSnap("t1", false, true), DiffOptions{})
			got := findingsWithID(rep, "index_invalid")
			if len(got) != 1 || got[0].Severity != SevCritical || !rep.Dangerous {
				t.Fatalf("index_invalid = %+v, dangerous=%v; want one critical finding", got, rep.Dangerous)
			}
		})
	}
}
