package erd

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Querier is the one pgx capability introspection needs (satisfied by
// *pgxpool.Pool and *pgx.Conn).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Structure only — names, types, key membership. No table data is ever read;
// pg_catalog is visible to any connecting role, so this needs nothing beyond
// CONNECT.
const columnsSQL = `
SELECT n.nspname  AS schema,
       c.relname  AS table,
       a.attname  AS column,
       format_type(a.atttypid, a.atttypmod) AS type,
       COALESCE((SELECT true FROM pg_index i
                 WHERE i.indrelid = c.oid AND i.indisprimary
                   AND a.attnum = ANY (i.indkey)), false) AS pk
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
WHERE c.relkind IN ('r', 'p')
  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND n.nspname NOT LIKE 'pg\_%'
  AND ($1 = '' OR n.nspname = $1)
ORDER BY n.nspname, c.relname, a.attnum`

const fksSQL = `
SELECT ns.nspname AS from_schema, s.relname AS from_table, sa.attname AS from_column,
       nt.nspname AS to_schema,   t.relname AS to_table,   ta.attname AS to_column
FROM pg_constraint con
JOIN pg_class s      ON s.oid = con.conrelid
JOIN pg_namespace ns ON ns.oid = s.relnamespace
JOIN pg_class t      ON t.oid = con.confrelid
JOIN pg_namespace nt ON nt.oid = t.relnamespace
JOIN LATERAL unnest(con.conkey)  WITH ORDINALITY AS ck(attnum, ord) ON true
JOIN LATERAL unnest(con.confkey) WITH ORDINALITY AS fk(attnum, ord) ON fk.ord = ck.ord
JOIN pg_attribute sa ON sa.attrelid = s.oid AND sa.attnum = ck.attnum
JOIN pg_attribute ta ON ta.attrelid = t.oid AND ta.attnum = fk.attnum
WHERE con.contype = 'f'
  AND ($1 = '' OR ns.nspname = $1)
ORDER BY 1, 2, 3`

// Introspect reads the schema structure for the diagram. schemaFilter narrows
// to one schema; "" means every user schema.
func Introspect(ctx context.Context, q Querier, schemaFilter string) (Schema, error) {
	var s Schema

	rows, err := q.Query(ctx, columnsSQL, schemaFilter)
	if err != nil {
		return s, fmt.Errorf("introspect columns: %w", err)
	}
	type colRow struct {
		Schema, Table, Column, Type string
		PK                          bool
	}
	byTable := map[string]*Table{}
	var order []string
	for rows.Next() {
		var r colRow
		if err := rows.Scan(&r.Schema, &r.Table, &r.Column, &r.Type, &r.PK); err != nil {
			rows.Close()
			return s, err
		}
		key := r.Schema + "." + r.Table
		t := byTable[key]
		if t == nil {
			t = &Table{Schema: r.Schema, Name: r.Table}
			byTable[key] = t
			order = append(order, key)
		}
		t.Columns = append(t.Columns, Column{Name: r.Column, Type: r.Type, PK: r.PK})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return s, err
	}

	rows, err = q.Query(ctx, fksSQL, schemaFilter)
	if err != nil {
		return s, fmt.Errorf("introspect foreign keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var fs, ft, fc, ts, tt, tc string
		if err := rows.Scan(&fs, &ft, &fc, &ts, &tt, &tc); err != nil {
			return s, err
		}
		s.Edges = append(s.Edges, Edge{FromTable: ft, FromColumn: fc, ToTable: tt, ToColumn: tc})
		if t := byTable[fs+"."+ft]; t != nil {
			for i := range t.Columns {
				if t.Columns[i].Name == fc {
					t.Columns[i].FKTarget = tt + "." + tc
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return s, err
	}

	for _, key := range order {
		s.Tables = append(s.Tables, *byTable[key])
	}
	introspectExtras(ctx, q, schemaFilter, &s)
	return s, nil
}

// Non-primary indexes, definitions compacted to method+columns (the PK marker
// already covers the primary index).
const indexesSQL = `
SELECT n.nspname AS schema, t.relname AS table, ic.relname AS index,
       regexp_replace(pg_get_indexdef(i.indexrelid), '^CREATE.*USING ', '') AS def,
       i.indisunique AS uniq
FROM pg_index i
JOIN pg_class ic     ON ic.oid = i.indexrelid
JOIN pg_class t      ON t.oid = i.indrelid
JOIN pg_namespace n  ON n.oid = t.relnamespace
WHERE NOT i.indisprimary
  AND t.relkind IN ('r', 'p')
  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND n.nspname NOT LIKE 'pg\_%'
  AND ($1 = '' OR n.nspname = $1)
ORDER BY 1, 2, 3`

// introspectExtras fills indexes and the database header info; both are
// best-effort decoration — an error leaves the structural diagram intact.
func introspectExtras(ctx context.Context, q Querier, schemaFilter string, s *Schema) {
	byKey := map[string]*Table{}
	for i := range s.Tables {
		byKey[s.Tables[i].Schema+"."+s.Tables[i].Name] = &s.Tables[i]
	}
	if rows, err := q.Query(ctx, indexesSQL, schemaFilter); err == nil {
		for rows.Next() {
			var sch, tbl, name, def string
			var uniq bool
			if rows.Scan(&sch, &tbl, &name, &def, &uniq) != nil {
				break
			}
			if t := byKey[sch+"."+tbl]; t != nil {
				t.Indexes = append(t.Indexes, Index{Name: name, Def: def, Unique: uniq})
			}
		}
		rows.Close()
	}
	if rows, err := q.Query(ctx, `SELECT current_database(), pg_database_size(current_database())`); err == nil {
		if rows.Next() {
			_ = rows.Scan(&s.Info.Database, &s.Info.SizeBytes)
		}
		rows.Close()
	}
}
