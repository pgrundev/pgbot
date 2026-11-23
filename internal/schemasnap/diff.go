package schemasnap

import (
	"fmt"
	"sort"
	"strings"
)

const (
	DiffKind    = "pgbot-schema-diff"
	DiffVersion = "1.0.0"

	CompareKind    = "pgbot-schema-compare"
	CompareVersion = "1.0.0"
)

// Severities, ordered. They match pgbot's finding severities.
const (
	SevInfo     = "info"
	SevWarning  = "warning"
	SevCritical = "critical"
)

// Finding is one consequence of a migration. ID is stable (agents and CI key
// on it); Detail and Advice are prose for a human or a model.
type Finding struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Object   string `json:"object"`
	Detail   string `json:"detail"`
	Advice   string `json:"advice,omitempty"`
	// Bytes: the size of the table the finding concerns, as measured on the
	// database the snapshots came from. Production may be larger.
	Bytes int64 `json:"bytes,omitempty"`
}

type DiffSummary struct {
	Critical int `json:"critical"`
	Warning  int `json:"warning"`
	Info     int `json:"info"`
}

type DiffReport struct {
	Kind      string      `json:"kind"`
	Version   string      `json:"version"`
	Database  string      `json:"database"`
	Before    string      `json:"before_captured_at"`
	After     string      `json:"after_captured_at"`
	Dangerous bool        `json:"dangerous"`
	Summary   DiffSummary `json:"summary"`
	Findings  []Finding   `json:"findings"`
	// Caveat travels with every report: the classification is what the
	// catalog proves, not everything a migration can do.
	Caveat string `json:"caveat"`
}

// DiffOptions tunes severities. LargeTableBytes: a rewrite, blocking index
// build or validating scan on a table at least this large is critical rather
// than a warning.
type DiffOptions struct {
	LargeTableBytes int64
}

const DefaultLargeTableBytes = 1 << 30

const diffCaveat = "Derived from two catalog snapshots of the same database. Lock and rewrite findings are " +
	"proven by the catalog (a changed relfilenode is a rewrite); whether an index was built CONCURRENTLY " +
	"cannot be seen afterwards, so new indexes on existing tables are flagged for review. Sizes are this " +
	"database's; production may be larger. Data-content changes (UPDATE/DELETE in a migration) are not visible."

// Diff classifies what a migration did, from snapshots taken before and after
// it ran on the same database.
func Diff(before, after *Snapshot, opt DiffOptions) DiffReport {
	if opt.LargeTableBytes <= 0 {
		opt.LargeTableBytes = DefaultLargeTableBytes
	}
	d := &differ{b: before, a: after, opt: opt}
	d.relations()
	d.columns()
	d.indexes()
	d.constraints()
	d.extensions()
	rep := DiffReport{Kind: DiffKind, Version: DiffVersion, Database: after.Database,
		Before: before.CapturedAt, After: after.CapturedAt, Findings: d.out, Caveat: diffCaveat}
	sortFindings(rep.Findings)
	for _, f := range rep.Findings {
		switch f.Severity {
		case SevCritical:
			rep.Summary.Critical++
		case SevWarning:
			rep.Summary.Warning++
		default:
			rep.Summary.Info++
		}
	}
	if rep.Findings == nil {
		rep.Findings = []Finding{}
	}
	rep.Dangerous = rep.Summary.Critical+rep.Summary.Warning > 0
	return rep
}

type differ struct {
	b, a *Snapshot
	opt  DiffOptions
	out  []Finding
}

func (d *differ) add(f Finding) { d.out = append(d.out, f) }

func relByOID(s *Snapshot) map[int64]Relation {
	m := make(map[int64]Relation, len(s.Relations))
	for _, r := range s.Relations {
		m[r.OID] = r
	}
	return m
}

func (r Relation) qname() string { return qual(r.Schema, r.Name) }

func (r Relation) hasStorage() bool { return r.Kind == "table" || r.Kind == "matview" }

// sizeSeverity: warning for a non-empty table, critical once it is large.
func (d *differ) sizeSeverity(bytes int64) string {
	if bytes >= d.opt.LargeTableBytes {
		return SevCritical
	}
	return SevWarning
}

func (d *differ) relations() {
	bm, am := relByOID(d.b), relByOID(d.a)
	for oid, br := range bm {
		ar, ok := am[oid]
		if !ok {
			if br.Kind == "view" || br.Kind == "sequence" {
				d.add(Finding{ID: br.Kind + "_dropped", Severity: SevWarning, Object: br.qname(),
					Detail: fmt.Sprintf("%s %s was dropped", br.Kind, br.qname()),
					Advice: "make sure no deployed code still uses it"})
				continue
			}
			d.add(Finding{ID: "table_dropped", Severity: SevCritical, Object: br.qname(), Bytes: br.Bytes,
				Detail: fmt.Sprintf("%s %s was dropped (%s of data on this database)", br.Kind, br.qname(), human(br.Bytes)),
				Advice: "irreversible data loss; drop in a later release after the code no longer reads it, and keep a backup"})
			continue
		}
		if br.Name != ar.Name || br.Schema != ar.Schema {
			d.add(Finding{ID: "table_renamed", Severity: SevWarning, Object: ar.qname(),
				Detail: fmt.Sprintf("%s renamed to %s", br.qname(), ar.qname()),
				Advice: "application code still using the old name fails between migrate and deploy; rename in steps (new name + view or code that reads both)"})
		}
		if br.hasStorage() && ar.hasStorage() && br.Relfilenode != 0 && ar.Relfilenode != br.Relfilenode && br.Bytes > 0 {
			d.add(Finding{ID: "table_rewritten", Severity: d.sizeSeverity(br.Bytes), Object: ar.qname(), Bytes: br.Bytes,
				Detail: fmt.Sprintf("%s was rewritten (%s): the migration held an ACCESS EXCLUSIVE lock, blocking reads and writes, for the whole rewrite", ar.qname(), human(br.Bytes)),
				Advice: "avoid a rewrite: add the column without a volatile default, change types in a new column and backfill in batches, or schedule a maintenance window"})
		}
	}
	for oid, ar := range am {
		if _, ok := bm[oid]; !ok {
			d.add(Finding{ID: ar.Kind + "_created", Severity: SevInfo, Object: ar.qname(),
				Detail: fmt.Sprintf("%s %s was created", ar.Kind, ar.qname())})
		}
	}
}

type colKey struct {
	rel    int64
	attnum int
}

func colMap(s *Snapshot) map[colKey]Column {
	m := make(map[colKey]Column, len(s.Columns))
	for _, c := range s.Columns {
		m[colKey{c.RelOID, c.Attnum}] = c
	}
	return m
}

func (d *differ) columns() {
	bm, am := colMap(d.b), colMap(d.a)
	brel, arel := relByOID(d.b), relByOID(d.a)
	for k, bc := range bm {
		r, relKept := arel[k.rel]
		if !relKept {
			continue // the table itself was dropped; reported once there
		}
		obj := qual(r.Schema, r.Name) + "." + bc.Name
		ac, ok := am[k]
		if !ok {
			if r.Kind == "view" {
				continue
			}
			d.add(Finding{ID: "column_dropped", Severity: SevCritical, Object: obj, Bytes: brel[k.rel].Bytes,
				Detail: fmt.Sprintf("column %s was dropped", obj),
				Advice: "irreversible data loss, and deployed code that still selects it fails; stop reading it in one release, drop it in a later one"})
			continue
		}
		if ac.Name != bc.Name {
			d.add(Finding{ID: "column_renamed", Severity: SevWarning, Object: qual(r.Schema, r.Name) + "." + ac.Name,
				Detail: fmt.Sprintf("column %s renamed to %s", obj, ac.Name),
				Advice: "code still using the old name fails between migrate and deploy; add the new column, write both, then drop the old one"})
		}
		if ac.Type != bc.Type {
			d.add(Finding{ID: "column_type_changed", Severity: SevWarning, Object: obj,
				Detail: fmt.Sprintf("column %s changed type %s → %s", obj, bc.Type, ac.Type),
				Advice: "check every reader of the column; a rewrite, if one happened, is reported separately"})
		}
		if ac.NotNull && !bc.NotNull && brel[k.rel].Bytes > 0 && r.Kind != "view" {
			d.add(Finding{ID: "not_null_added", Severity: d.sizeSeverity(brel[k.rel].Bytes), Object: obj, Bytes: brel[k.rel].Bytes,
				Detail: fmt.Sprintf("SET NOT NULL on %s scanned the existing table (%s) under an ACCESS EXCLUSIVE lock", obj, human(brel[k.rel].Bytes)),
				Advice: "add a CHECK (col IS NOT NULL) NOT VALID constraint, VALIDATE it separately, then SET NOT NULL (PostgreSQL 12+ skips the scan)"})
		}
	}
	for k, ac := range am {
		if _, ok := bm[k]; ok {
			continue
		}
		r, existed := brel[k.rel]
		if !existed || r.Kind == "view" {
			continue // a column of a new table
		}
		obj := qual(r.Schema, r.Name) + "." + ac.Name
		sev, advice := SevInfo, ""
		detail := fmt.Sprintf("column %s was added (%s)", obj, ac.Type)
		if ac.NotNull && !ac.HasDefault && ac.Identity == "" && ac.Generated == "" {
			sev = SevWarning
			advice = "a NOT NULL column without a default makes every INSERT from not-yet-deployed code fail"
		}
		d.add(Finding{ID: "column_added", Severity: sev, Object: obj, Detail: detail, Advice: advice})
	}
}

func (d *differ) indexes() {
	bIdx := map[int64]Index{}
	for _, i := range d.b.Indexes {
		bIdx[i.OID] = i
	}
	aIdx := map[int64]Index{}
	for _, i := range d.a.Indexes {
		aIdx[i.OID] = i
	}
	brel := relByOID(d.b)
	for oid, ai := range aIdx {
		bi, had := bIdx[oid]
		switch {
		case !ai.Valid && had && !bi.Valid:
			// Already INVALID before: debris the reviewed migration did not
			// leave, so it is noted but does not make the migration dangerous.
			d.add(Finding{ID: "index_invalid", Severity: SevInfo, Object: qual(ai.Schema, ai.Name),
				Detail: fmt.Sprintf("index %s was already INVALID before this migration (not caused by it)", qual(ai.Schema, ai.Name)),
				Advice: "it is maintained on every write but never used; DROP INDEX CONCURRENTLY it and build it again"})
		case !ai.Valid:
			d.add(Finding{ID: "index_invalid", Severity: SevCritical, Object: qual(ai.Schema, ai.Name),
				Detail: fmt.Sprintf("index %s is INVALID (a failed CREATE INDEX CONCURRENTLY leaves one behind)", qual(ai.Schema, ai.Name)),
				Advice: "it is maintained on every write but never used; DROP INDEX CONCURRENTLY it and build it again"})
		}
		if had {
			continue
		}
		r, existed := brel[ai.RelOID]
		if !existed || r.Bytes == 0 {
			continue // index on a new or empty table: nothing to block
		}
		d.add(Finding{ID: "index_created_on_existing_table", Severity: d.sizeSeverity(r.Bytes), Object: qual(ai.Schema, ai.Name), Bytes: r.Bytes,
			Detail: fmt.Sprintf("index %s was built on existing table %s (%s); a plain CREATE INDEX blocks writes to the table until the build finishes", qual(ai.Schema, ai.Name), r.qname(), human(r.Bytes)),
			Advice: "use CREATE INDEX CONCURRENTLY (Rails: algorithm: :concurrently + disable_ddl_transaction!) unless the table is small in production too"})
	}
	for oid, bi := range bIdx {
		if _, ok := aIdx[oid]; ok {
			continue
		}
		if _, relKept := relByOID(d.a)[bi.RelOID]; !relKept {
			continue
		}
		d.add(Finding{ID: "index_dropped", Severity: SevWarning, Object: qual(bi.Schema, bi.Name),
			Detail: fmt.Sprintf("index %s was dropped", qual(bi.Schema, bi.Name)),
			Advice: "queries that used it fall back to scans; check pgbot indexes / queries on production before dropping, and use DROP INDEX CONCURRENTLY"})
	}
}

func (d *differ) constraints() {
	bCon := map[int64]Constraint{}
	for _, c := range d.b.Constraints {
		bCon[c.OID] = c
	}
	brel, arel := relByOID(d.b), relByOID(d.a)
	for _, ac := range d.a.Constraints {
		bc, existed := bCon[ac.OID]
		r, relExisted := brel[ac.RelOID]
		obj := ac.Name
		if ar, ok := arel[ac.RelOID]; ok {
			obj = ar.qname() + "." + ac.Name
		}
		if !existed && relExisted && r.Bytes > 0 && ac.Validated && (ac.Type == "foreign_key" || ac.Type == "check") {
			d.add(Finding{ID: "constraint_validated_on_existing_table", Severity: d.sizeSeverity(r.Bytes), Object: obj, Bytes: r.Bytes,
				Detail: fmt.Sprintf("%s constraint %s was added and validated against existing rows of %s (%s) while holding a lock", strings.ReplaceAll(ac.Type, "_", " "), ac.Name, r.qname(), human(r.Bytes)),
				Advice: "add it NOT VALID, then ALTER TABLE … VALIDATE CONSTRAINT in a separate step (a weaker lock that allows writes)"})
		}
		if existed && !bc.Validated && ac.Validated {
			d.add(Finding{ID: "constraint_validated", Severity: SevInfo, Object: obj,
				Detail: fmt.Sprintf("constraint %s was validated (SHARE UPDATE EXCLUSIVE: writes continue)", ac.Name)})
		}
	}
}

func (d *differ) extensions() {
	bm := map[string]string{}
	for _, e := range d.b.Extensions {
		bm[e.Name] = e.Version
	}
	am := map[string]string{}
	for _, e := range d.a.Extensions {
		am[e.Name] = e.Version
	}
	for n, v := range am {
		if bv, ok := bm[n]; !ok {
			d.add(Finding{ID: "extension_created", Severity: SevInfo, Object: n, Detail: fmt.Sprintf("extension %s %s was created", n, v),
				Advice: "make sure production (and every branch engine you use) has it available"})
		} else if bv != v {
			d.add(Finding{ID: "extension_updated", Severity: SevInfo, Object: n, Detail: fmt.Sprintf("extension %s updated %s → %s", n, bv, v)})
		}
	}
	for n := range bm {
		if _, ok := am[n]; !ok {
			d.add(Finding{ID: "extension_dropped", Severity: SevWarning, Object: n, Detail: fmt.Sprintf("extension %s was dropped", n)})
		}
	}
}

var sevRank = map[string]int{SevCritical: 0, SevWarning: 1, SevInfo: 2}

func sortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		if sevRank[fs[i].Severity] != sevRank[fs[j].Severity] {
			return sevRank[fs[i].Severity] < sevRank[fs[j].Severity]
		}
		if fs[i].ID != fs[j].ID {
			return fs[i].ID < fs[j].ID
		}
		return fs[i].Object < fs[j].Object
	})
}

func human(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
