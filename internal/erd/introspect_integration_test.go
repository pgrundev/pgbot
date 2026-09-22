package erd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestIntegrationIntrospectPreservesQualifiedTableIdentity(t *testing.T) {
	dsn := os.Getenv("PGBOT_TEST_SUPERUSER_DSN")
	if dsn == "" {
		t.Skip("set PGBOT_TEST_SUPERUSER_DSN to run the disposable cross-schema ERD introspection test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	prefix := fmt.Sprintf("pgbot_erd_%d", time.Now().UnixNano())
	collisionA := tableIdentity{Schema: prefix + ".part", Name: "users"}
	collisionB := tableIdentity{Schema: prefix, Name: "part.users"}
	sameName := tableIdentity{Schema: prefix + ` "客户"`, Name: "users"}
	child := tableIdentity{Schema: prefix + " sales", Name: "orders"}
	schemas := []string{collisionA.Schema, collisionB.Schema, sameName.Schema, child.Schema}
	var createdSchemas []string
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		for i := len(createdSchemas) - 1; i >= 0; i-- {
			if _, err := conn.Exec(cleanupCtx, `DROP SCHEMA IF EXISTS `+pgx.Identifier{createdSchemas[i]}.Sanitize()+` CASCADE`); err != nil {
				t.Errorf("drop schema %q: %v", createdSchemas[i], err)
			}
		}
	})
	for _, schema := range schemas {
		if _, err := conn.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
			t.Fatalf("create schema %q: %v", schema, err)
		}
		createdSchemas = append(createdSchemas, schema)
	}

	createTable := func(id tableIdentity, body string) {
		t.Helper()
		if _, err := conn.Exec(ctx, `CREATE TABLE `+pgx.Identifier{id.Schema, id.Name}.Sanitize()+` (`+body+`)`); err != nil {
			t.Fatalf("create table %s: %v", qualifiedTableName(id), err)
		}
	}
	createTable(collisionA, `id bigint PRIMARY KEY, a_marker text`)
	createTable(collisionB, `id bigint PRIMARY KEY, b_marker text`)
	createTable(sameName, `id bigint PRIMARY KEY`)
	createTable(child, `id bigint PRIMARY KEY, buyer_id bigint REFERENCES `+pgx.Identifier{collisionA.Schema, collisionA.Name}.Sanitize()+` (id)`)
	for id, index := range map[tableIdentity]string{collisionA: "a_marker_idx", collisionB: "b_marker_idx"} {
		column := strings.TrimSuffix(index, "_idx")
		if _, err := conn.Exec(ctx, `CREATE INDEX `+pgx.Identifier{index}.Sanitize()+` ON `+pgx.Identifier{id.Schema, id.Name}.Sanitize()+` (`+pgx.Identifier{column}.Sanitize()+`)`); err != nil {
			t.Fatalf("create index %q: %v", index, err)
		}
	}

	schema, err := Introspect(ctx, conn, "")
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	byID := map[tableIdentity]Table{}
	for _, table := range schema.Tables {
		byID[identityOf(table)] = table
	}
	for id, marker := range map[tableIdentity]string{collisionA: "a_marker", collisionB: "b_marker"} {
		table, ok := byID[id]
		if !ok {
			t.Errorf("missing table %s", qualifiedTableName(id))
			continue
		}
		found := false
		for _, column := range table.Columns {
			found = found || column.Name == marker
		}
		if !found {
			t.Errorf("table %s lost its distinct %q column: %+v", qualifiedTableName(id), marker, table.Columns)
		}
		wantIndex := marker + "_idx"
		found = false
		for _, index := range table.Indexes {
			found = found || index.Name == wantIndex
		}
		if !found {
			t.Errorf("table %s lost its distinct %q index: %+v", qualifiedTableName(id), wantIndex, table.Indexes)
		}
	}

	var relationship *Edge
	for i := range schema.Edges {
		edge := &schema.Edges[i]
		if edge.FromSchema == child.Schema && edge.FromTable == child.Name && edge.FromColumn == "buyer_id" {
			relationship = edge
			break
		}
	}
	if relationship == nil {
		t.Fatal("missing cross-schema foreign-key edge")
	}
	if relationship.ToSchema != collisionA.Schema || relationship.ToTable != collisionA.Name || relationship.ToColumn != "id" {
		t.Errorf("foreign-key target lost schema identity: %+v", *relationship)
	}
	childTable := byID[child]
	for _, column := range childTable.Columns {
		if column.Name == "buyer_id" {
			want := qualifiedTableName(collisionA) + ".id"
			if column.FKTarget != want {
				t.Errorf("FK label = %q, want %q", column.FKTarget, want)
			}
			return
		}
	}
	t.Fatal("child table lost buyer_id column")
}
