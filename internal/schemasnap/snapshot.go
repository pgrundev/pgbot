// Package schemasnap captures a database's schema structure as a JSON
// snapshot (catalog only: never a row of table data) and compares two
// snapshots offline.
//
// Two comparisons are supported:
//
//   - Diff(before, after): the same database before and after a migration.
//     Object identity is the catalog OID, so a rename is told apart from a
//     drop+create, and a changed relfilenode on a table that existed before
//     means the migration rewrote it (an ACCESS EXCLUSIVE lock held for the
//     whole rewrite). This is what flags dangerous migrations.
//   - Compare(a, b): the same migration applied on two different servers
//     (for example PGX and stock PostgreSQL). OIDs mean nothing across
//     servers, so objects are matched by name and only structure is compared.
//
// Everything here is read from pg_catalog; the connecting role needs nothing
// beyond CONNECT.
package schemasnap

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/pgrundev/pggo"
)

// SnapshotKind / SnapshotVersion identify the JSON document. Bump the minor
// for additive fields, the major for anything a consumer must handle.
const (
	SnapshotKind    = "pgbot-schema-snapshot"
	SnapshotVersion = "1.0.0"
)

// Querier is the one driver capability the snapshot needs (*pggo.Pool,
// *pggo.Conn and *pggo.Tx satisfy it).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (*pggo.Rows, error)
}

type Snapshot struct {
	Kind          string       `json:"kind"`
	Version       string       `json:"version"`
	CapturedAt    string       `json:"captured_at"`
	Database      string       `json:"database"`
	ServerVersion string       `json:"server_version"`
	Relations     []Relation   `json:"relations"`
	Columns       []Column     `json:"columns"`
	Indexes       []Index      `json:"indexes"`
	Constraints   []Constraint `json:"constraints"`
	Extensions    []Extension  `json:"extensions"`
}

// Relation is a table, partitioned table, materialized view, view or sequence.
type Relation struct {
	OID         int64   `json:"oid"`
	Schema      string  `json:"schema"`
	Name        string  `json:"name"`
	Kind        string  `json:"kind"` // table | partitioned_table | matview | view | sequence
	Relfilenode int64   `json:"relfilenode"`
	Bytes       int64   `json:"bytes"`    // main fork size; 0 for views
	EstRows     float64 `json:"est_rows"` // pg_class.reltuples; -1 = never analyzed
}

type Column struct {
	RelOID     int64  `json:"rel_oid"`
	Attnum     int    `json:"attnum"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	NotNull    bool   `json:"not_null"`
	HasDefault bool   `json:"has_default"`
	Identity   string `json:"identity,omitempty"`  // "a" always, "d" by default
	Generated  string `json:"generated,omitempty"` // "s" stored, "v" virtual
}

type Index struct {
	OID        int64  `json:"oid"`
	RelOID     int64  `json:"rel_oid"`
	Schema     string `json:"schema"`
	Name       string `json:"name"`
	Definition string `json:"definition"`
	Unique     bool   `json:"unique"`
	Primary    bool   `json:"primary"`
	Valid      bool   `json:"valid"`
}

type Constraint struct {
	OID        int64  `json:"oid"`
	RelOID     int64  `json:"rel_oid"`
	Name       string `json:"name"`
	Type       string `json:"type"` // primary_key | unique | foreign_key | check | exclusion | not_null | trigger
	Validated  bool   `json:"validated"`
	Definition string `json:"definition"`
}

type Extension struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

const userSchemas = `n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
  AND n.nspname NOT LIKE 'pg\_temp\_%' AND n.nspname NOT LIKE 'pg\_toast\_temp\_%'`

var relationsSQL = `
SELECT c.oid::bigint, n.nspname, c.relname, c.relkind::text, c.relfilenode::bigint,
       CASE WHEN c.relkind IN ('r', 'm', 'S') THEN pg_relation_size(c.oid) ELSE 0 END::bigint,
       c.reltuples::float8
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p', 'm', 'v', 'S')
  AND ` + userSchemas + `
ORDER BY n.nspname, c.relname`

var columnsSQL = `
SELECT a.attrelid::bigint, a.attnum::int, a.attname, format_type(a.atttypid, a.atttypmod),
       a.attnotnull, a.atthasdef, a.attidentity::text, a.attgenerated::text
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE a.attnum > 0 AND NOT a.attisdropped
  AND c.relkind IN ('r', 'p', 'm', 'v')
  AND ` + userSchemas + `
ORDER BY a.attrelid, a.attnum`

var indexesSQL = `
SELECT i.indexrelid::bigint, i.indrelid::bigint, n.nspname, ic.relname,
       pg_get_indexdef(i.indexrelid), i.indisunique, i.indisprimary, i.indisvalid
FROM pg_index i
JOIN pg_class ic ON ic.oid = i.indexrelid
JOIN pg_namespace n ON n.oid = ic.relnamespace
WHERE ` + userSchemas + `
ORDER BY n.nspname, ic.relname`

var constraintsSQL = `
SELECT con.oid::bigint, con.conrelid::bigint, con.conname, con.contype::text, con.convalidated,
       pg_get_constraintdef(con.oid)
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE con.conrelid <> 0
  AND ` + userSchemas + `
ORDER BY con.conrelid, con.conname`

const extensionsSQL = `SELECT extname, extversion FROM pg_extension ORDER BY extname`

var relKinds = map[string]string{"r": "table", "p": "partitioned_table", "m": "matview", "v": "view", "S": "sequence"}

var conTypes = map[string]string{"p": "primary_key", "u": "unique", "f": "foreign_key", "c": "check",
	"x": "exclusion", "n": "not_null", "t": "trigger"}

// Take reads the snapshot. It runs five catalog queries and reads no table data.
func Take(ctx context.Context, q Querier) (*Snapshot, error) {
	s := &Snapshot{Kind: SnapshotKind, Version: SnapshotVersion, CapturedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := scanAll(ctx, q, `SELECT current_database(), current_setting('server_version')`, func(r *pggo.Rows) error {
		return r.Scan(&s.Database, &s.ServerVersion)
	}); err != nil {
		return nil, fmt.Errorf("schema snapshot: database: %w", err)
	}
	if err := scanAll(ctx, q, relationsSQL, func(r *pggo.Rows) error {
		var x Relation
		if err := r.Scan(&x.OID, &x.Schema, &x.Name, &x.Kind, &x.Relfilenode, &x.Bytes, &x.EstRows); err != nil {
			return err
		}
		if k, ok := relKinds[x.Kind]; ok {
			x.Kind = k
		}
		s.Relations = append(s.Relations, x)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("schema snapshot: relations: %w", err)
	}
	if err := scanAll(ctx, q, columnsSQL, func(r *pggo.Rows) error {
		var x Column
		if err := r.Scan(&x.RelOID, &x.Attnum, &x.Name, &x.Type, &x.NotNull, &x.HasDefault, &x.Identity, &x.Generated); err != nil {
			return err
		}
		s.Columns = append(s.Columns, x)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("schema snapshot: columns: %w", err)
	}
	if err := scanAll(ctx, q, indexesSQL, func(r *pggo.Rows) error {
		var x Index
		if err := r.Scan(&x.OID, &x.RelOID, &x.Schema, &x.Name, &x.Definition, &x.Unique, &x.Primary, &x.Valid); err != nil {
			return err
		}
		s.Indexes = append(s.Indexes, x)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("schema snapshot: indexes: %w", err)
	}
	if err := scanAll(ctx, q, constraintsSQL, func(r *pggo.Rows) error {
		var x Constraint
		if err := r.Scan(&x.OID, &x.RelOID, &x.Name, &x.Type, &x.Validated, &x.Definition); err != nil {
			return err
		}
		if t, ok := conTypes[x.Type]; ok {
			x.Type = t
		}
		s.Constraints = append(s.Constraints, x)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("schema snapshot: constraints: %w", err)
	}
	if err := scanAll(ctx, q, extensionsSQL, func(r *pggo.Rows) error {
		var x Extension
		if err := r.Scan(&x.Name, &x.Version); err != nil {
			return err
		}
		s.Extensions = append(s.Extensions, x)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("schema snapshot: extensions: %w", err)
	}
	s.normalize()
	return s, nil
}

func scanAll(ctx context.Context, q Querier, sql string, each func(*pggo.Rows) error) error {
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := each(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// normalize gives every slice a deterministic order and turns nil slices into
// empty ones, so two snapshots of an identical schema serialize identically.
func (s *Snapshot) normalize() {
	if s.Relations == nil {
		s.Relations = []Relation{}
	}
	if s.Columns == nil {
		s.Columns = []Column{}
	}
	if s.Indexes == nil {
		s.Indexes = []Index{}
	}
	if s.Constraints == nil {
		s.Constraints = []Constraint{}
	}
	if s.Extensions == nil {
		s.Extensions = []Extension{}
	}
	sort.Slice(s.Relations, func(i, j int) bool {
		return qual(s.Relations[i].Schema, s.Relations[i].Name) < qual(s.Relations[j].Schema, s.Relations[j].Name)
	})
	sort.Slice(s.Columns, func(i, j int) bool {
		if s.Columns[i].RelOID != s.Columns[j].RelOID {
			return s.Columns[i].RelOID < s.Columns[j].RelOID
		}
		return s.Columns[i].Attnum < s.Columns[j].Attnum
	})
	sort.Slice(s.Indexes, func(i, j int) bool {
		return qual(s.Indexes[i].Schema, s.Indexes[i].Name) < qual(s.Indexes[j].Schema, s.Indexes[j].Name)
	})
	sort.Slice(s.Constraints, func(i, j int) bool {
		if s.Constraints[i].RelOID != s.Constraints[j].RelOID {
			return s.Constraints[i].RelOID < s.Constraints[j].RelOID
		}
		return s.Constraints[i].Name < s.Constraints[j].Name
	})
	sort.Slice(s.Extensions, func(i, j int) bool { return s.Extensions[i].Name < s.Extensions[j].Name })
}

func qual(schema, name string) string { return schema + "." + name }
