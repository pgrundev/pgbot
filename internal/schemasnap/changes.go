package schemasnap

import (
	"fmt"
	"sort"
	"strings"
)

// A changeset is the complete, severity-free list of what one branch's
// migrations changed in the schema, relative to a snapshot of the branch it
// targets. Diff answers "is this migration dangerous to run?"; a changeset
// answers "which objects does this branch touch?", which is what two branches
// need to be checked against each other (see Conflicts).
const (
	ChangesetKind    = "pgbot-schema-changeset"
	ChangesetVersion = "1.0.0"
)

// Change ops.
const (
	OpCreated = "created"
	OpDropped = "dropped"
	OpRenamed = "renamed"
	OpAltered = "altered"
)

// Change kinds. Relations keep their own kind (table, view, matview,
// partitioned_table, sequence).
const (
	KindColumn     = "column"
	KindIndex      = "index"
	KindConstraint = "constraint"
	KindExtension  = "extension"
)

// Change is one object a branch created, dropped, renamed or altered.
//
// Object is the object's name after the change (before it, for a drop).
// From is the old name of a rename. Table is the owning relation
// (schema.table, as named after the change) for columns, indexes and
// constraints, and the relation itself for relations. Def is the object's
// definition after the change (column shape, index or constraint definition)
// so two branches creating the same name can be told identical or not.
type Change struct {
	Op     string `json:"op"`
	Kind   string `json:"kind"`
	Object string `json:"object"`
	From   string `json:"from,omitempty"`
	Table  string `json:"table,omitempty"`
	Def    string `json:"def,omitempty"`
	Detail string `json:"detail"`
	// References: the relations (and relation.column pairs) a new or changed
	// foreign key points at, so a branch dropping or renaming them conflicts.
	References []string `json:"references,omitempty"`
}

type Changeset struct {
	Kind     string   `json:"kind"`
	Version  string   `json:"version"`
	Label    string   `json:"label"`           // who made the change: "#123", a branch name
	Title    string   `json:"title,omitempty"` // free text, e.g. the PR title
	Ref      string   `json:"ref,omitempty"`   // commit the changes were measured at
	URL      string   `json:"url,omitempty"`   // where to read about it (the PR)
	Database string   `json:"database"`
	Before   string   `json:"before_captured_at"`
	After    string   `json:"after_captured_at"`
	Changes  []Change `json:"changes"`
}

// ChangesetMeta labels a changeset.
type ChangesetMeta struct {
	Label, Title, Ref, URL string
}

// Changes lists everything that differs between before and after.
//
// Objects are matched by name first, so before and after may come from
// different databases (a branch of the base migrated separately). What is
// left unmatched on both sides is then matched by catalog identity (OID, or a
// column's attnum within a matched table), which turns a drop+create pair
// into a rename when both snapshots share a catalog lineage: the same
// database, or branches copied from one.
func Changes(before, after *Snapshot, meta ChangesetMeta) Changeset {
	c := &changer{b: before, a: after}
	c.relations()
	c.columns()
	c.indexes()
	c.constraints()
	c.extensions()
	sortChanges(c.out)
	if c.out == nil {
		c.out = []Change{}
	}
	return Changeset{Kind: ChangesetKind, Version: ChangesetVersion, Label: meta.Label, Title: meta.Title,
		Ref: meta.Ref, URL: meta.URL, Database: after.Database, Before: before.CapturedAt, After: after.CapturedAt,
		Changes: c.out}
}

type changer struct {
	b, a *Snapshot
	out  []Change
	// relation OID mapping between the two snapshots, rename-aware
	relB2A map[int64]int64
	relA2B map[int64]int64
	bRel   map[int64]Relation
	aRel   map[int64]Relation
}

func (c *changer) add(x Change) { c.out = append(c.out, x) }

// matchByName pairs before/after items by name, then pairs what is left on
// both sides by identity. It returns the pairs and the unmatched remainder.
func matchByName[T any](before, after []T, name func(T) string, id func(T) int64) (pairs [][2]T, gone, born []T) {
	byName := map[string]int{}
	for i, x := range after {
		byName[name(x)] = i
	}
	usedA := make([]bool, len(after))
	var leftB []T
	for _, x := range before {
		if i, ok := byName[name(x)]; ok && !usedA[i] {
			usedA[i] = true
			pairs = append(pairs, [2]T{x, after[i]})
			continue
		}
		leftB = append(leftB, x)
	}
	byID := map[int64]int{}
	for i, x := range after {
		if !usedA[i] {
			byID[id(x)] = i
		}
	}
	for _, x := range leftB {
		if i, ok := byID[id(x)]; ok && id(x) != 0 && !usedA[i] {
			usedA[i] = true
			pairs = append(pairs, [2]T{x, after[i]})
			continue
		}
		gone = append(gone, x)
	}
	for i, x := range after {
		if !usedA[i] {
			born = append(born, x)
		}
	}
	return pairs, gone, born
}

func (c *changer) relations() {
	c.relB2A, c.relA2B = map[int64]int64{}, map[int64]int64{}
	c.bRel, c.aRel = relByOID(c.b), relByOID(c.a)
	pairs, gone, born := matchByName(c.b.Relations, c.a.Relations,
		func(r Relation) string { return r.qname() }, func(r Relation) int64 { return r.OID })
	for _, p := range pairs {
		br, ar := p[0], p[1]
		c.relB2A[br.OID], c.relA2B[ar.OID] = ar.OID, br.OID
		if br.qname() != ar.qname() {
			c.add(Change{Op: OpRenamed, Kind: ar.Kind, Object: ar.qname(), From: br.qname(), Table: ar.qname(),
				Detail: fmt.Sprintf("%s %s renamed to %s", ar.Kind, br.qname(), ar.qname())})
		}
		if br.Kind != ar.Kind {
			c.add(Change{Op: OpAltered, Kind: ar.Kind, Object: ar.qname(), Table: ar.qname(), Def: ar.Kind,
				Detail: fmt.Sprintf("%s changed from %s to %s", ar.qname(), br.Kind, ar.Kind)})
		} else if br.OID == ar.OID && br.hasStorage() && br.Relfilenode != 0 && ar.Relfilenode != br.Relfilenode && br.Bytes > 0 {
			c.add(Change{Op: OpAltered, Kind: ar.Kind, Object: ar.qname(), Table: ar.qname(),
				Detail: fmt.Sprintf("%s was rewritten", ar.qname())})
		}
	}
	for _, r := range gone {
		c.add(Change{Op: OpDropped, Kind: r.Kind, Object: r.qname(), Table: r.qname(),
			Detail: fmt.Sprintf("%s %s dropped", r.Kind, r.qname())})
	}
	newTables := map[string]bool{}
	for _, r := range born {
		if r.Kind != "sequence" {
			newTables[r.qname()] = true
		}
	}
	for _, r := range born {
		table := r.qname()
		if r.Kind == "sequence" {
			table = serialOwner(r, newTables)
		}
		c.add(Change{Op: OpCreated, Kind: r.Kind, Object: r.qname(), Table: table, Def: r.Kind,
			Detail: fmt.Sprintf("%s %s created", r.Kind, r.qname())})
	}
}

// serialOwner: a sequence named <table>_<column>_seq created together with
// <table> is that table's serial/identity sequence, part of creating it.
func serialOwner(seq Relation, newTables map[string]bool) string {
	if strings.HasSuffix(seq.Name, "_seq") {
		for t := range newTables {
			if strings.HasPrefix(seq.qname(), t+"_") {
				return t
			}
		}
	}
	return seq.qname()
}

func columnsByRel(s *Snapshot) map[int64][]Column {
	m := map[int64][]Column{}
	for _, col := range s.Columns {
		m[col.RelOID] = append(m[col.RelOID], col)
	}
	return m
}

func (c *changer) columns() {
	bc, ac := columnsByRel(c.b), columnsByRel(c.a)
	for bOID, aOID := range c.relB2A {
		ar := c.aRel[aOID]
		pairs, gone, born := matchByName(bc[bOID], ac[aOID],
			func(x Column) string { return x.Name }, func(x Column) int64 { return int64(x.Attnum) })
		obj := func(name string) string { return ar.qname() + "." + name }
		for _, p := range pairs {
			bx, ax := p[0], p[1]
			if bx.Name != ax.Name {
				c.add(Change{Op: OpRenamed, Kind: KindColumn, Object: obj(ax.Name), From: obj(bx.Name), Table: ar.qname(),
					Def: columnShape(ax), Detail: fmt.Sprintf("column %s renamed to %s", obj(bx.Name), ax.Name)})
			}
			if what := columnDelta(bx, ax); what != "" {
				c.add(Change{Op: OpAltered, Kind: KindColumn, Object: obj(ax.Name), Table: ar.qname(),
					Def: columnShape(ax), Detail: fmt.Sprintf("column %s: %s", obj(ax.Name), what)})
			}
		}
		for _, x := range gone {
			c.add(Change{Op: OpDropped, Kind: KindColumn, Object: obj(x.Name), Table: ar.qname(),
				Detail: fmt.Sprintf("column %s dropped", obj(x.Name))})
		}
		for _, x := range born {
			c.add(Change{Op: OpCreated, Kind: KindColumn, Object: obj(x.Name), Table: ar.qname(),
				Def: columnShape(x), Detail: fmt.Sprintf("column %s added (%s)", obj(x.Name), columnShape(x))})
		}
	}
}

// columnDelta describes how a column's shape changed, "" when it did not.
func columnDelta(b, a Column) string {
	var parts []string
	if b.Type != a.Type {
		parts = append(parts, fmt.Sprintf("type %s → %s", b.Type, a.Type))
	}
	if b.NotNull != a.NotNull {
		parts = append(parts, map[bool]string{true: "NOT NULL added", false: "NOT NULL dropped"}[a.NotNull])
	}
	if b.HasDefault != a.HasDefault {
		parts = append(parts, map[bool]string{true: "default added", false: "default dropped"}[a.HasDefault])
	}
	if b.Identity != a.Identity {
		parts = append(parts, "identity changed")
	}
	if b.Generated != a.Generated {
		parts = append(parts, "generation changed")
	}
	return strings.Join(parts, ", ")
}

// indexShape is an index definition without its own name or its table's name
// (both reported separately), so a table rename is not an index change.
func indexShape(i Index) string {
	def := i.Definition
	if k := strings.Index(def, " USING "); k >= 0 {
		def = def[k+1:]
	}
	if i.Unique {
		def = "UNIQUE " + def
	}
	return def
}

func (c *changer) tableName(s *Snapshot, relOID int64) string {
	if s == c.a {
		if r, ok := c.aRel[relOID]; ok {
			return r.qname()
		}
		return ""
	}
	// a before-side relation, named as it is after the change when it survived
	if a, ok := c.relB2A[relOID]; ok {
		return c.aRel[a].qname()
	}
	if r, ok := c.bRel[relOID]; ok {
		return r.qname()
	}
	return ""
}

func (c *changer) indexes() {
	pairs, gone, born := matchByName(c.b.Indexes, c.a.Indexes,
		func(i Index) string { return qual(i.Schema, i.Name) }, func(i Index) int64 { return i.OID })
	for _, p := range pairs {
		bi, ai := p[0], p[1]
		table := c.tableName(c.a, ai.RelOID)
		if qual(bi.Schema, bi.Name) != qual(ai.Schema, ai.Name) {
			c.add(Change{Op: OpRenamed, Kind: KindIndex, Object: qual(ai.Schema, ai.Name), From: qual(bi.Schema, bi.Name),
				Table: table, Def: indexShape(ai), Detail: fmt.Sprintf("index %s renamed to %s", qual(bi.Schema, bi.Name), ai.Name)})
		}
		if indexShape(bi) != indexShape(ai) || c.relB2A[bi.RelOID] != ai.RelOID {
			c.add(Change{Op: OpAltered, Kind: KindIndex, Object: qual(ai.Schema, ai.Name), Table: table, Def: indexShape(ai),
				Detail: fmt.Sprintf("index %s redefined: %s", qual(ai.Schema, ai.Name), indexShape(ai))})
		}
	}
	for _, i := range gone {
		if _, kept := c.relB2A[i.RelOID]; !kept {
			continue // went with its table
		}
		c.add(Change{Op: OpDropped, Kind: KindIndex, Object: qual(i.Schema, i.Name), Table: c.tableName(c.b, i.RelOID),
			Detail: fmt.Sprintf("index %s dropped", qual(i.Schema, i.Name))})
	}
	for _, i := range born {
		if _, existed := c.relA2B[i.RelOID]; !existed {
			continue // part of a new table
		}
		c.add(Change{Op: OpCreated, Kind: KindIndex, Object: qual(i.Schema, i.Name), Table: c.tableName(c.a, i.RelOID),
			Def: indexShape(i), Detail: fmt.Sprintf("index %s created: %s", qual(i.Schema, i.Name), indexShape(i))})
	}
}

// constraintsOf keys a snapshot's constraints by "schema.table.name" with the
// table named as it is after the change. NOT NULL constraints (PostgreSQL 18
// catalogs them) are left to the column shape, which every version has.
func (c *changer) constraintsOf(s *Snapshot) []namedConstraint {
	var out []namedConstraint
	for _, x := range s.Constraints {
		if x.Type == "not_null" {
			continue
		}
		t := c.tableName(s, x.RelOID)
		if t == "" {
			continue
		}
		out = append(out, namedConstraint{Constraint: x, table: t})
	}
	return out
}

type namedConstraint struct {
	Constraint
	table string
}

func (n namedConstraint) key() string { return n.table + "." + n.Name }

func (c *changer) constraints() {
	pairs, gone, born := matchByName(c.constraintsOf(c.b), c.constraintsOf(c.a),
		namedConstraint.key, func(n namedConstraint) int64 { return n.OID })
	for _, p := range pairs {
		bx, ax := p[0], p[1]
		if bx.key() != ax.key() {
			c.add(Change{Op: OpRenamed, Kind: KindConstraint, Object: ax.key(), From: bx.key(), Table: ax.table,
				Def: ax.Type + " " + ax.Definition, References: c.references(ax),
				Detail: fmt.Sprintf("constraint %s renamed to %s", bx.key(), ax.Name)})
		}
		if bx.Type != ax.Type || bx.Definition != ax.Definition {
			c.add(Change{Op: OpAltered, Kind: KindConstraint, Object: ax.key(), Table: ax.table,
				Def: ax.Type + " " + ax.Definition, References: c.references(ax),
				Detail: fmt.Sprintf("constraint %s redefined: %s", ax.key(), ax.Definition)})
		} else if !bx.Validated && ax.Validated {
			c.add(Change{Op: OpAltered, Kind: KindConstraint, Object: ax.key(), Table: ax.table,
				Def: ax.Type + " " + ax.Definition, Detail: fmt.Sprintf("constraint %s validated", ax.key())})
		}
	}
	for _, x := range gone {
		if _, kept := c.relB2A[x.RelOID]; !kept {
			continue
		}
		c.add(Change{Op: OpDropped, Kind: KindConstraint, Object: x.key(), Table: x.table,
			Detail: fmt.Sprintf("constraint %s dropped", x.key())})
	}
	for _, x := range born {
		// Constraints of a new table are part of creating it, except foreign
		// keys: those point at other tables, which other branches may change.
		if _, existed := c.relA2B[x.RelOID]; !existed && x.Type != "foreign_key" {
			continue
		}
		c.add(Change{Op: OpCreated, Kind: KindConstraint, Object: x.key(), Table: x.table,
			Def: x.Type + " " + x.Definition, References: c.references(x),
			Detail: fmt.Sprintf("constraint %s added: %s", x.key(), x.Definition)})
	}
}

// references resolves the relation (and columns) a foreign key points at,
// from pg_get_constraintdef's "REFERENCES [schema.]table(col, …)".
func (c *changer) references(n namedConstraint) []string {
	if n.Type != "foreign_key" {
		return nil
	}
	k := strings.Index(n.Definition, "REFERENCES ")
	if k < 0 {
		return nil
	}
	rest := n.Definition[k+len("REFERENCES "):]
	open := strings.IndexByte(rest, '(')
	closeIdx := strings.IndexByte(rest, ')')
	if open <= 0 || closeIdx < open {
		return nil
	}
	rel := strings.Trim(strings.TrimSpace(rest[:open]), `"`)
	if !strings.Contains(rel, ".") {
		// unqualified: it is on the search_path; resolve against the snapshot
		rel = c.resolveRel(rel)
	}
	out := []string{rel}
	for _, col := range strings.Split(rest[open+1:closeIdx], ",") {
		out = append(out, rel+"."+strings.Trim(strings.TrimSpace(col), `"`))
	}
	return out
}

func (c *changer) resolveRel(name string) string {
	var hit string
	for _, r := range c.a.Relations {
		if r.Name == name {
			if r.Schema == "public" {
				return r.qname()
			}
			if hit == "" {
				hit = r.qname()
			}
		}
	}
	if hit != "" {
		return hit
	}
	return "public." + name
}

func (c *changer) extensions() {
	bm := map[string]string{}
	for _, e := range c.b.Extensions {
		bm[e.Name] = e.Version
	}
	am := map[string]string{}
	for _, e := range c.a.Extensions {
		am[e.Name] = e.Version
	}
	for n, v := range am {
		if bv, ok := bm[n]; !ok {
			c.add(Change{Op: OpCreated, Kind: KindExtension, Object: n, Def: v, Detail: fmt.Sprintf("extension %s %s created", n, v)})
		} else if bv != v {
			c.add(Change{Op: OpAltered, Kind: KindExtension, Object: n, Def: v, Detail: fmt.Sprintf("extension %s %s → %s", n, bv, v)})
		}
	}
	for n := range bm {
		if _, ok := am[n]; !ok {
			c.add(Change{Op: OpDropped, Kind: KindExtension, Object: n, Detail: fmt.Sprintf("extension %s dropped", n)})
		}
	}
}

func sortChanges(cs []Change) {
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].Table != cs[j].Table {
			return cs[i].Table < cs[j].Table
		}
		if cs[i].Object != cs[j].Object {
			return cs[i].Object < cs[j].Object
		}
		return cs[i].Op < cs[j].Op
	})
}
