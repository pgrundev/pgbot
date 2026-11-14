package schemasnap

import (
	"fmt"
	"sort"
	"strings"
)

// Difference is one structural mismatch between two servers' snapshots.
// A or B is "" when the object is missing on that side.
type Difference struct {
	Object string `json:"object"`
	Field  string `json:"field"`
	A      string `json:"a"`
	B      string `json:"b"`
}

type CompareReport struct {
	Kind        string       `json:"kind"`
	Version     string       `json:"version"`
	ALabel      string       `json:"a_label"`
	BLabel      string       `json:"b_label"`
	AServer     string       `json:"a_server_version"`
	BServer     string       `json:"b_server_version"`
	Identical   bool         `json:"identical"`
	Differences []Difference `json:"differences"`
	Caveat      string       `json:"caveat"`
}

const compareCaveat = "Structure only (relations, columns, indexes, constraints, extensions), matched by name. " +
	"Catalog identity (OIDs, relfilenodes), sizes and row estimates differ between servers by design and are ignored. " +
	"Identical structure does not prove identical query behaviour."

type relShape struct {
	kind    string
	columns []string
}

func shapes(s *Snapshot) (map[string]relShape, map[string]Index, map[string]Constraint, map[string]string) {
	byOID := relByOID(s)
	rels := map[string]relShape{}
	for _, r := range s.Relations {
		rels[r.qname()] = relShape{kind: r.Kind}
	}
	for _, c := range s.Columns {
		r, ok := byOID[c.RelOID]
		if !ok {
			continue
		}
		sh := rels[r.qname()]
		sh.columns = append(sh.columns, columnShape(c))
		rels[r.qname()] = sh
	}
	idx := map[string]Index{}
	for _, i := range s.Indexes {
		idx[qual(i.Schema, i.Name)] = i
	}
	cons := map[string]Constraint{}
	for _, c := range s.Constraints {
		if c.Type == "not_null" {
			continue // PostgreSQL 18 catalogs NOT NULL as constraints, earlier versions do not; the column shape carries it
		}
		if r, ok := byOID[c.RelOID]; ok {
			cons[r.qname()+"."+c.Name] = c
		}
	}
	ext := map[string]string{}
	for _, e := range s.Extensions {
		ext[e.Name] = e.Version
	}
	return rels, idx, cons, ext
}

func columnShape(c Column) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", c.Name, c.Type)
	if c.NotNull {
		b.WriteString(" not null")
	}
	if c.HasDefault {
		b.WriteString(" default")
	}
	if c.Identity != "" {
		b.WriteString(" identity:" + c.Identity)
	}
	if c.Generated != "" {
		b.WriteString(" generated:" + c.Generated)
	}
	return b.String()
}

// Compare reports every structural difference between two servers' snapshots
// of what should be the same schema.
func Compare(a, b *Snapshot, aLabel, bLabel string) CompareReport {
	ar, ai, ac, ae := shapes(a)
	br, bi, bc, be := shapes(b)
	var diffs []Difference
	add := func(obj, field, x, y string) {
		diffs = append(diffs, Difference{Object: obj, Field: field, A: x, B: y})
	}
	for _, name := range unionKeys(ar, br) {
		x, xok := ar[name]
		y, yok := br[name]
		switch {
		case !xok:
			add(name, "relation", "", y.kind)
		case !yok:
			add(name, "relation", x.kind, "")
		default:
			if x.kind != y.kind {
				add(name, "kind", x.kind, y.kind)
			}
			if strings.Join(x.columns, ", ") != strings.Join(y.columns, ", ") {
				add(name, "columns", strings.Join(x.columns, ", "), strings.Join(y.columns, ", "))
			}
		}
	}
	for _, name := range unionKeys(ai, bi) {
		x, xok := ai[name]
		y, yok := bi[name]
		switch {
		case !xok:
			add(name, "index", "", y.Definition)
		case !yok:
			add(name, "index", x.Definition, "")
		default:
			if x.Definition != y.Definition {
				add(name, "index_definition", x.Definition, y.Definition)
			}
			if x.Valid != y.Valid {
				add(name, "index_valid", fmt.Sprint(x.Valid), fmt.Sprint(y.Valid))
			}
		}
	}
	for _, name := range unionKeys(ac, bc) {
		x, xok := ac[name]
		y, yok := bc[name]
		switch {
		case !xok:
			add(name, "constraint", "", y.Definition)
		case !yok:
			add(name, "constraint", x.Definition, "")
		default:
			if x.Type != y.Type || x.Definition != y.Definition {
				add(name, "constraint_definition", x.Type+" "+x.Definition, y.Type+" "+y.Definition)
			}
			if x.Validated != y.Validated {
				add(name, "constraint_validated", fmt.Sprint(x.Validated), fmt.Sprint(y.Validated))
			}
		}
	}
	for _, name := range unionKeys(ae, be) {
		x, xok := ae[name]
		y, yok := be[name]
		if !xok || !yok {
			add(name, "extension", x, y)
		}
	}
	if diffs == nil {
		diffs = []Difference{}
	}
	return CompareReport{Kind: CompareKind, Version: CompareVersion, ALabel: aLabel, BLabel: bLabel,
		AServer: a.ServerVersion, BServer: b.ServerVersion, Identical: len(diffs) == 0, Differences: diffs, Caveat: compareCaveat}
}

func unionKeys[V any](a, b map[string]V) []string {
	seen := map[string]bool{}
	var out []string
	for k := range a {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range b {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
