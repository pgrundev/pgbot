package collect_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/collect"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pggo"
)

// Issue #129: a partial index whose predicate is `<fk col> IS NOT NULL` covers
// the FK — the RI trigger's lookup (`WHERE $1 = fk_col`, strict operator)
// implies the predicate, so even the generic plan uses it. A partial index on
// any other predicate (e.g. soft-delete) does not: the RI check must still see
// the rows it excludes. Proves the planner fact on the server, then runs the
// real collector as the read-only role.
func TestIntegration_fkPartialIndexCoverage(t *testing.T) {
	su := os.Getenv("PGBOT_TEST_SUPERUSER_DSN")
	if su == "" {
		t.Skip("set PGBOT_TEST_SUPERUSER_DSN (a superuser DSN) to run the FK partial-index fixture")
	}
	ro := dsn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pggo.Connect(ctx, su)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	cleanup := func() { _, _ = admin.Exec(context.Background(), `DROP SCHEMA IF EXISTS pgbot_it_fk CASCADE`) }
	cleanup()
	t.Cleanup(cleanup)
	if _, err := admin.SimpleQuery(ctx, `
		CREATE SCHEMA pgbot_it_fk;
		GRANT USAGE ON SCHEMA pgbot_it_fk TO PUBLIC;
		CREATE TABLE pgbot_it_fk.p (id bigint PRIMARY KEY);
		CREATE TABLE pgbot_it_fk.p2 (x bigint, y bigint, PRIMARY KEY (x, y));
		INSERT INTO pgbot_it_fk.p SELECT g FROM generate_series(1, 100) g;
		-- covered: predicate is exactly the FK column IS NOT NULL
		CREATE TABLE pgbot_it_fk.c_notnull (id bigint PRIMARY KEY, parent_id bigint REFERENCES pgbot_it_fk.p(id));
		CREATE INDEX c_notnull_parent ON pgbot_it_fk.c_notnull (parent_id) WHERE parent_id IS NOT NULL;
		-- covered: quoted FK column name, multi-column conjunction
		CREATE TABLE pgbot_it_fk.c_multi (id bigint PRIMARY KEY, "Ref A" bigint, b bigint,
			FOREIGN KEY ("Ref A", b) REFERENCES pgbot_it_fk.p2(x, y));
		CREATE INDEX ON pgbot_it_fk.c_multi ("Ref A", b) WHERE "Ref A" IS NOT NULL AND b IS NOT NULL;
		-- NOT covered: soft-delete predicate hides rows the RI check must see
		CREATE TABLE pgbot_it_fk.c_softdel (id bigint PRIMARY KEY, parent_id bigint REFERENCES pgbot_it_fk.p(id), deleted_at timestamptz);
		CREATE INDEX ON pgbot_it_fk.c_softdel (parent_id) WHERE deleted_at IS NULL;
		-- NOT covered: IS NOT NULL mixed with another condition
		CREATE TABLE pgbot_it_fk.c_mixed (id bigint PRIMARY KEY, parent_id bigint REFERENCES pgbot_it_fk.p(id));
		CREATE INDEX ON pgbot_it_fk.c_mixed (parent_id) WHERE parent_id IS NOT NULL AND id > 10;
		INSERT INTO pgbot_it_fk.c_notnull SELECT g, CASE WHEN g % 3 = 0 THEN NULL ELSE g % 100 + 1 END FROM generate_series(1, 2000) g;
		ANALYZE pgbot_it_fk.c_notnull`); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	// The premise, confirmed against the server: the RI check's query shape uses
	// the partial index even as a generic (parameter-agnostic) plan.
	if _, err := admin.SimpleQuery(ctx, `
		SET plan_cache_mode = force_generic_plan;
		SET enable_seqscan = off;
		PREPARE pgbot_it_ri(bigint) AS
			SELECT 1 FROM ONLY pgbot_it_fk.c_notnull x WHERE $1 OPERATOR(pg_catalog.=) parent_id FOR KEY SHARE OF x`); err != nil {
		t.Fatalf("prepare RI-shaped query: %v", err)
	}
	var plan string
	if err := admin.QueryRow(ctx, `EXPLAIN (FORMAT JSON) EXECUTE pgbot_it_ri(5)`).Scan(&plan); err != nil {
		t.Fatalf("explain: %v", err)
	}
	if !strings.Contains(plan, "c_notnull_parent") {
		t.Fatalf("premise: the RI lookup should use the IS NOT NULL partial index, plan: %s", plan)
	}

	target, err := conn.Connect(ctx, ro)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer target.Close()
	c, err := collect.Run(ctx, target, collect.Options{Interval: 200 * time.Millisecond, ASHHz: 0})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if c.Indexes == nil {
		t.Fatal("indexes section missing")
	}
	got := map[string]bool{}
	for _, fk := range c.Indexes.UnindexedFKs {
		if fk.Schema == "pgbot_it_fk" {
			got[fk.Table] = true
		}
	}
	for table, wantUnindexed := range map[string]bool{
		"c_notnull": false,
		"c_multi":   false,
		"c_softdel": true,
		"c_mixed":   true,
	} {
		if got[table] != wantUnindexed {
			t.Errorf("%s: reported unindexed = %v, want %v (all: %+v)", table, got[table], wantUnindexed, c.Indexes.UnindexedFKs)
		}
	}
}
