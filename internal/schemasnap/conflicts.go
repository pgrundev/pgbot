package schemasnap

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	ConflictsKind    = "pgbot-schema-conflicts"
	ConflictsVersion = "1.0.0"

	// SevConflict: merging both branches breaks — the second one's migration
	// fails (the object already exists, or no longer exists), or it silently
	// changes what the other one relies on.
	SevConflict = "conflict"
	// SevOverlap: both branches change the same table in different ways. Each
	// can be fine alone; the merge order matters and the schema dump will
	// conflict, so someone should look before both land.
	SevOverlap = "overlap"
)

// Conflict is one place where two branches' schema changes collide.
type Conflict struct {
	Severity string   `json:"severity"`
	A        string   `json:"a"` // changeset labels
	B        string   `json:"b"`
	Object   string   `json:"object"` // the contested object (an overlap: the table)
	Table    string   `json:"table,omitempty"`
	AChanges []string `json:"a_changes"` // what each side does to it
	BChanges []string `json:"b_changes"`
	Reason   string   `json:"reason"`
}

type ChangesetRef struct {
	Label   string `json:"label"`
	Title   string `json:"title,omitempty"`
	Ref     string `json:"ref,omitempty"`
	URL     string `json:"url,omitempty"`
	After   string `json:"after_captured_at,omitempty"`
	Changes int    `json:"changes"`
}

type ConflictSummary struct {
	Conflicts int `json:"conflicts"`
	Overlaps  int `json:"overlaps"`
}

type ConflictReport struct {
	Kind       string          `json:"kind"`
	Version    string          `json:"version"`
	Focus      string          `json:"focus,omitempty"`
	Changesets []ChangesetRef  `json:"changesets"`
	Summary    ConflictSummary `json:"summary"`
	Items      []Conflict      `json:"items"`
	Caveat     string          `json:"caveat"`
}

const conflictsCaveat = "Compares the schema changes each branch's migrations made against its target branch " +
	"(catalog only). It cannot see data migrations, application code that reads a changed column, or " +
	"changes made on a branch since its changeset was last recorded. A clean result means no two " +
	"branches touch the same objects, not that they work together."

// Conflicts checks every pair of changesets. With focus set (a label), only
// pairs that include that changeset are checked — "does my branch collide
// with any other open branch?".
func Conflicts(sets []*Changeset, focus string) ConflictReport {
	rep := ConflictReport{Kind: ConflictsKind, Version: ConflictsVersion, Focus: focus, Caveat: conflictsCaveat}
	for _, s := range sets {
		rep.Changesets = append(rep.Changesets, ChangesetRef{Label: s.Label, Title: s.Title, Ref: s.Ref, URL: s.URL,
			After: s.After, Changes: len(s.Changes)})
	}
	for i := 0; i < len(sets); i++ {
		for j := i + 1; j < len(sets); j++ {
			a, b := sets[i], sets[j]
			if focus != "" && b.Label == focus {
				a, b = b, a // the focused branch always reads as "A"
			}
			if focus != "" && a.Label != focus {
				continue
			}
			rep.Items = append(rep.Items, pairConflicts(a, b)...)
		}
	}
	for _, it := range rep.Items {
		if it.Severity == SevConflict {
			rep.Summary.Conflicts++
		} else {
			rep.Summary.Overlaps++
		}
	}
	if rep.Items == nil {
		rep.Items = []Conflict{}
	}
	if rep.Changesets == nil {
		rep.Changesets = []ChangesetRef{}
	}
	return rep
}

func isRelationKind(k string) bool {
	switch k {
	case KindColumn, KindIndex, KindConstraint, KindExtension:
		return false
	}
	return true
}

// kindClass folds the relation kinds together: a view and a table cannot
// share a name either.
func kindClass(k string) string {
	if isRelationKind(k) {
		return "relation"
	}
	return k
}

// names: every name a change makes or takes away.
func (x Change) names() []string {
	if x.Op == OpRenamed {
		return []string{x.From, x.Object}
	}
	return []string{x.Object}
}

// removes: the name this change takes away, "" when it takes none.
func (x Change) removes() string {
	switch x.Op {
	case OpDropped:
		return x.Object
	case OpRenamed:
		return x.From
	}
	return ""
}

// tables: the relation names a change is about (old and new, for a rename).
func (x Change) tables() []string {
	if x.Table == "" {
		return nil
	}
	out := []string{x.Table}
	if x.Op == OpRenamed && isRelationKind(x.Kind) {
		out = append(out, x.From)
	}
	return out
}

func intersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y && x != "" {
				return true
			}
		}
	}
	return false
}

type pairKey struct{ object string }

func pairConflicts(a, b *Changeset) []Conflict {
	found := map[pairKey]*Conflict{}
	var order []pairKey
	conflictTables := map[string]bool{}
	record := func(object, table, reason string, x, y Change) {
		k := pairKey{object}
		c, ok := found[k]
		if !ok {
			c = &Conflict{Severity: SevConflict, A: a.Label, B: b.Label, Object: object, Table: table, Reason: reason}
			found[k] = c
			order = append(order, k)
		}
		c.AChanges = appendUnique(c.AChanges, x.Detail)
		c.BChanges = appendUnique(c.BChanges, y.Detail)
		for _, t := range append(x.tables(), y.tables()...) {
			conflictTables[t] = true
		}
	}

	aNew, bNew := createdTables(a), createdTables(b)
	for _, x := range a.Changes {
		for _, y := range b.Changes {
			// 1. Both change the same object.
			if kindClass(x.Kind) == kindClass(y.Kind) && intersects(x.names(), y.names()) {
				if x.Kind == KindExtension && x.Op == OpCreated && y.Op == OpCreated {
					continue // CREATE EXTENSION IF NOT EXISTS is the norm; the overlap pass reports nothing for it
				}
				obj := x.Object
				if y.removes() != "" && intersects([]string{y.removes()}, x.names()) {
					obj = y.removes()
				} else if x.removes() != "" {
					obj = x.removes()
				}
				if x.Table != x.Object && x.Table == y.Table && aNew[x.Table] && bNew[y.Table] {
					obj = x.Table // a serial sequence or foreign key of a table both create: part of that table's conflict
				}
				record(obj, x.Table, sameObjectReason(x, y, a.Label, b.Label), x, y)
				continue
			}
			// 2. One takes away something the other builds on.
			if obj, why := dependsOnRemoved(x, y, a.Label, b.Label); obj != "" {
				record(obj, x.Table, why, x, y)
				continue
			}
			if obj, why := dependsOnRemoved(y, x, b.Label, a.Label); obj != "" {
				record(obj, y.Table, why, x, y)
			}
		}
	}

	out := make([]Conflict, 0, len(order))
	for _, k := range order {
		out = append(out, *found[k])
	}

	// 3. Both change the same table, in different objects.
	aByTable, bByTable := changesByTable(a), changesByTable(b)
	var shared []string
	for t := range aByTable {
		if _, ok := bByTable[t]; ok && !conflictTables[t] {
			shared = append(shared, t)
		}
	}
	sort.Strings(shared)
	for _, t := range shared {
		out = append(out, Conflict{Severity: SevOverlap, A: a.Label, B: b.Label, Object: t, Table: t,
			AChanges: details(aByTable[t]), BChanges: details(bByTable[t]),
			Reason: fmt.Sprintf("%s and %s both change this table: whichever merges second must re-run its migrations on top of the first and regenerate the schema dump", a.Label, b.Label)})
	}
	return out
}

func verb(op string) string {
	switch op {
	case OpCreated:
		return "creates"
	case OpDropped:
		return "drops"
	case OpRenamed:
		return "renames"
	}
	return "changes"
}

// sameObjectReason: x (from branch lx) and y (from ly) touch one object.
// createdTables: the relations a changeset creates.
func createdTables(s *Changeset) map[string]bool {
	m := map[string]bool{}
	for _, x := range s.Changes {
		if x.Op == OpCreated && isRelationKind(x.Kind) && x.Table == x.Object {
			m[x.Object] = true
		}
	}
	return m
}

func sameObjectReason(x, y Change, lx, ly string) string {
	switch {
	case x.Op == OpCreated && y.Op == OpCreated:
		if x.Def == y.Def {
			return fmt.Sprintf("%s and %s both create it, identically: whichever migration runs second fails with \"already exists\" (or, behind IF NOT EXISTS, two migrations own one object)", lx, ly)
		}
		return fmt.Sprintf("%s and %s both create it, differently (%s vs %s): whichever migration runs second fails with \"already exists\"", lx, ly, x.Def, y.Def)
	case x.removes() != "" && y.removes() != "":
		return fmt.Sprintf("%s %s it and %s %s it: whichever migration runs second fails because it no longer exists", lx, verb(x.Op), ly, verb(y.Op))
	case x.removes() != "":
		return fmt.Sprintf("%s %s it while %s %s it: whichever migration runs second fails, or lands on the wrong object", lx, verb(x.Op), ly, verb(y.Op))
	case y.removes() != "":
		return fmt.Sprintf("%s %s it while %s %s it: whichever migration runs second fails, or lands on the wrong object", ly, verb(y.Op), lx, verb(x.Op))
	default:
		return fmt.Sprintf("%s and %s both change it: whichever merges second runs its migration against a shape its author never tested", lx, ly)
	}
}

// dependsOnRemoved: does y (from branch ly) build on a name that x (from lx)
// takes away? It returns the contested object and why, or "" when not.
func dependsOnRemoved(x, y Change, lx, ly string) (string, string) {
	gone := x.removes()
	if gone == "" {
		return "", ""
	}
	if isRelationKind(x.Kind) {
		if !isRelationKind(y.Kind) && y.Table == gone {
			return gone, fmt.Sprintf("%s %s %s while %s %s its %s %s: whichever migration runs second fails", lx, verb(x.Op), gone, ly, verb(y.Op), y.Kind, y.Object)
		}
		if contains(y.References, gone) {
			return gone, fmt.Sprintf("%s %s %s while %s adds a foreign key to it: whichever migration runs second fails", lx, verb(x.Op), gone, ly)
		}
		return "", ""
	}
	if x.Kind != KindColumn {
		return "", ""
	}
	if contains(y.References, gone) {
		return gone, fmt.Sprintf("%s %s column %s while %s adds a foreign key that references it: whichever migration runs second fails", lx, verb(x.Op), gone, ly)
	}
	table, col := splitColumn(gone)
	if y.Table == table && (y.Kind == KindIndex || y.Kind == KindConstraint) && y.Op != OpDropped && mentionsColumn(y.Def, col) {
		return gone, fmt.Sprintf("%s %s column %s while %s's %s %s uses it: whichever migration runs second fails", lx, verb(x.Op), col, ly, y.Kind, y.Object)
	}
	return "", ""
}

func splitColumn(qualified string) (table, col string) {
	k := strings.LastIndexByte(qualified, '.')
	if k < 0 {
		return "", qualified
	}
	return qualified[:k], qualified[k+1:]
}

func mentionsColumn(def, col string) bool {
	if def == "" || col == "" {
		return false
	}
	return regexp.MustCompile(`(^|[^A-Za-z0-9_$"])"?` + regexp.QuoteMeta(col) + `"?([^A-Za-z0-9_$"]|$)`).MatchString(def)
}

func changesByTable(s *Changeset) map[string][]Change {
	m := map[string][]Change{}
	for _, x := range s.Changes {
		for _, t := range x.tables() {
			if x.Op == OpCreated && isRelationKind(x.Kind) {
				continue // a table one branch creates is not "shared" with anyone else's; same-name creation is a conflict above
			}
			m[t] = append(m[t], x)
		}
	}
	return m
}

func details(cs []Change) []string {
	var out []string
	for _, x := range cs {
		out = appendUnique(out, x.Detail)
	}
	return out
}

func appendUnique(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}
	return append(xs, s)
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
