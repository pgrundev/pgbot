package schemasnap

import (
	"strings"
	"testing"
)

func snap(rels []Relation, cols []Column, idx []Index, cons []Constraint, ext ...Extension) *Snapshot {
	s := &Snapshot{Kind: SnapshotKind, Version: SnapshotVersion, Relations: rels, Columns: cols, Indexes: idx,
		Constraints: cons, Extensions: ext}
	s.normalize()
	return s
}

func baseSnap() *Snapshot {
	return snap(
		[]Relation{
			{OID: 100, Schema: "public", Name: "users", Kind: "table", Relfilenode: 100, Bytes: 8192},
			{OID: 200, Schema: "public", Name: "orders", Kind: "table", Relfilenode: 200, Bytes: 8192},
		},
		[]Column{
			{RelOID: 100, Attnum: 1, Name: "id", Type: "bigint", NotNull: true},
			{RelOID: 100, Attnum: 2, Name: "email", Type: "text"},
			{RelOID: 200, Attnum: 1, Name: "id", Type: "bigint", NotNull: true},
			{RelOID: 200, Attnum: 2, Name: "user_id", Type: "bigint"},
		},
		[]Index{{OID: 101, RelOID: 100, Schema: "public", Name: "users_pkey", Definition: "CREATE UNIQUE INDEX users_pkey ON public.users USING btree (id)", Unique: true, Primary: true, Valid: true}},
		[]Constraint{{OID: 102, RelOID: 100, Name: "users_pkey", Type: "primary_key", Validated: true, Definition: "PRIMARY KEY (id)"}},
		Extension{Name: "plpgsql", Version: "1.0"},
	)
}

// clone copies a snapshot so a test can apply a "migration" to it.
func clone(s *Snapshot) *Snapshot {
	c := *s
	c.Relations = append([]Relation(nil), s.Relations...)
	c.Columns = append([]Column(nil), s.Columns...)
	c.Indexes = append([]Index(nil), s.Indexes...)
	c.Constraints = append([]Constraint(nil), s.Constraints...)
	c.Extensions = append([]Extension(nil), s.Extensions...)
	return &c
}

func TestChangesRenameByIdentityAndNameMatchAcrossLineages(t *testing.T) {
	b := baseSnap()
	a := clone(b)
	a.Columns[1].Name = "email_address" // same attnum: a rename
	a.Columns = append(a.Columns, Column{RelOID: 100, Attnum: 3, Name: "plan", Type: "text", NotNull: true, HasDefault: true})
	a.normalize()
	cs := Changes(b, a, ChangesetMeta{Label: "x"})
	if !hasChange(cs, OpRenamed, KindColumn, "public.users.email_address") || len(cs.Changes) != 2 {
		t.Fatalf("got %+v", cs.Changes)
	}

	// A separately migrated database: every OID differs, names still match.
	other := clone(b)
	for i := range other.Relations {
		other.Relations[i].OID += 5000
	}
	for i := range other.Columns {
		other.Columns[i].RelOID += 5000
	}
	for i := range other.Indexes {
		other.Indexes[i].OID += 5000
		other.Indexes[i].RelOID += 5000
	}
	for i := range other.Constraints {
		other.Constraints[i].OID += 5000
		other.Constraints[i].RelOID += 5000
	}
	if cs := Changes(b, other, ChangesetMeta{}); len(cs.Changes) != 0 {
		t.Fatalf("same schema, different OIDs: want no changes, got %+v", cs.Changes)
	}
}

func TestChangesNewTableSerialSequenceBelongsToTable(t *testing.T) {
	b := baseSnap()
	a := clone(b)
	a.Relations = append(a.Relations,
		Relation{OID: 300, Schema: "public", Name: "invoices", Kind: "table"},
		Relation{OID: 301, Schema: "public", Name: "invoices_id_seq", Kind: "sequence"})
	a.normalize()
	for _, c := range Changes(b, a, ChangesetMeta{}).Changes {
		if c.Object == "public.invoices_id_seq" && c.Table != "public.invoices" {
			t.Errorf("serial sequence not attached to its table: %+v", c)
		}
	}
}

func cs(label string, changes ...Change) *Changeset {
	return &Changeset{Kind: ChangesetKind, Label: label, Changes: changes}
}

func TestConflictRules(t *testing.T) {
	addCol := func(obj, def string) Change {
		return Change{Op: OpCreated, Kind: KindColumn, Object: obj, Table: obj[:strings.LastIndexByte(obj, '.')], Def: def, Detail: "add " + obj}
	}
	cases := []struct {
		name     string
		a, b     *Changeset
		severity string // "" = nothing
		object   string
	}{
		{"same column added identically", cs("A", addCol("public.users.plan", "plan text")), cs("B", addCol("public.users.plan", "plan text")), SevConflict, "public.users.plan"},
		{"different columns on one table", cs("A", addCol("public.users.plan", "plan text")), cs("B", addCol("public.users.tier", "tier int")), SevOverlap, "public.users"},
		{"different tables", cs("A", addCol("public.users.plan", "plan text")), cs("B", addCol("public.orders.note", "note text")), "", ""},
		{"table dropped vs its column changed",
			cs("A", Change{Op: OpDropped, Kind: "table", Object: "public.orders", Table: "public.orders", Detail: "drop"}),
			cs("B", addCol("public.orders.note", "note text")), SevConflict, "public.orders"},
		{"table renamed vs fk to it",
			cs("A", Change{Op: OpRenamed, Kind: "table", Object: "public.customers", From: "public.users", Table: "public.customers", Detail: "rename"}),
			cs("B", Change{Op: OpCreated, Kind: KindConstraint, Object: "public.orders.fk", Table: "public.orders", References: []string{"public.users", "public.users.id"}, Detail: "fk"}),
			SevConflict, "public.users"},
		{"column dropped vs index on it",
			cs("A", Change{Op: OpDropped, Kind: KindColumn, Object: "public.users.email", Table: "public.users", Detail: "drop"}),
			cs("B", Change{Op: OpCreated, Kind: KindIndex, Object: "public.users_email_idx", Table: "public.users", Def: "USING btree (lower(email))", Detail: "idx"}),
			SevConflict, "public.users.email"},
		{"column dropped vs index on a column sharing a prefix",
			cs("A", Change{Op: OpDropped, Kind: KindColumn, Object: "public.users.email", Table: "public.users", Detail: "drop"}),
			cs("B", Change{Op: OpCreated, Kind: KindIndex, Object: "public.users_email2_idx", Table: "public.users", Def: "USING btree (email2)", Detail: "idx"}),
			SevOverlap, "public.users"},
		{"both enable one extension",
			cs("A", Change{Op: OpCreated, Kind: KindExtension, Object: "pg_trgm", Def: "1.6", Detail: "ext"}),
			cs("B", Change{Op: OpCreated, Kind: KindExtension, Object: "pg_trgm", Def: "1.6", Detail: "ext"}), "", ""},
		{"index and table share a name class? no: index vs column with same text",
			cs("A", Change{Op: OpCreated, Kind: KindIndex, Object: "public.users.x", Table: "public.users", Detail: "i"}),
			cs("B", Change{Op: OpCreated, Kind: "table", Object: "public.users.x", Table: "public.users.x", Detail: "t"}), "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := Conflicts([]*Changeset{tc.a, tc.b}, "")
			if tc.severity == "" {
				if len(rep.Items) != 0 {
					t.Fatalf("want nothing, got %s", itemKeys(rep.Items))
				}
				return
			}
			if len(rep.Items) != 1 || rep.Items[0].Severity != tc.severity || rep.Items[0].Object != tc.object {
				t.Fatalf("want one %s on %s, got %s", tc.severity, tc.object, itemKeys(rep.Items))
			}
			if !strings.Contains(rep.Items[0].Reason, "A") || !strings.Contains(rep.Items[0].Reason, "B") {
				t.Errorf("reason does not name both branches: %q", rep.Items[0].Reason)
			}
		})
	}
}

func TestConflictsFocusPutsFocusFirst(t *testing.T) {
	a := cs("#1", Change{Op: OpCreated, Kind: KindColumn, Object: "public.t.c", Table: "public.t", Detail: "a"})
	b := cs("#2", Change{Op: OpCreated, Kind: KindColumn, Object: "public.t.c", Table: "public.t", Detail: "b"})
	c := cs("#3", Change{Op: OpCreated, Kind: KindColumn, Object: "public.t.c", Table: "public.t", Detail: "c"})
	rep := Conflicts([]*Changeset{a, b, c}, "#2")
	if len(rep.Items) != 2 {
		t.Fatalf("want #2↔#1 and #2↔#3, got %s", itemKeys(rep.Items))
	}
	for _, it := range rep.Items {
		if it.A != "#2" || it.AChanges[0] != "b" {
			t.Errorf("focus not on the A side: %+v", it)
		}
	}
	if all := Conflicts([]*Changeset{a, b, c}, ""); len(all.Items) != 3 {
		t.Errorf("unfocused: want 3 pairs, got %s", itemKeys(all.Items))
	}
}
