package schemasnap

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pggo"
)

// Runs against a real server when PGBOT_TEST_SUPERUSER_DSN is set (it creates
// and drops a scratch database, so it needs CREATEDB). Every migration shape
// the diff classifies is applied for real and the snapshot taken from the live
// catalog, so the relfilenode/attnum/OID logic is tested against PostgreSQL's
// actual behaviour, not a model of it.
func TestDiffAgainstRealMigrations(t *testing.T) {
	dsn := os.Getenv("PGBOT_TEST_SUPERUSER_DSN")
	if dsn == "" {
		t.Skip("PGBOT_TEST_SUPERUSER_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, err := pggo.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	db := "pgbot_schemasnap_" + time.Now().Format("150405")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")

	cfg, err := pggo.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = db
	c, err := pggo.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	exec := func(sql string) {
		t.Helper()
		if _, err := c.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`CREATE TABLE users (id bigserial PRIMARY KEY, email text, name text, legacy text)`)
	exec(`INSERT INTO users (email, name, legacy) SELECT 'u'||g||'@x', 'n'||g, 'l' FROM generate_series(1, 5000) g`)
	exec(`CREATE TABLE orders (id bigserial PRIMARY KEY, user_id bigint, total int)`)
	exec(`INSERT INTO orders (user_id, total) SELECT (g % 5000) + 1, g FROM generate_series(1, 5000) g`)
	exec(`CREATE TABLE scratch (id int)`)
	exec(`INSERT INTO scratch VALUES (1)`)
	exec(`CREATE INDEX orders_total_idx ON orders (total)`)

	before, err := Take(ctx, c)
	if err != nil {
		t.Fatal(err)
	}

	// The "migration": one of every shape.
	exec(`ALTER TABLE orders ALTER COLUMN total TYPE bigint`)                                            // rewrite
	exec(`CREATE INDEX users_email_idx ON users (email)`)                                                // blocking build on existing table
	exec(`ALTER TABLE orders ADD CONSTRAINT orders_user_fk FOREIGN KEY (user_id) REFERENCES users (id)`) // validating scan
	exec(`ALTER TABLE users ALTER COLUMN name SET NOT NULL`)                                             // scan under lock
	exec(`ALTER TABLE users DROP COLUMN legacy`)                                                         // data loss
	exec(`ALTER TABLE users RENAME COLUMN email TO email_address`)                                       // breaks old code
	exec(`ALTER TABLE users ADD COLUMN plan text NOT NULL DEFAULT 'free'`)                               // safe (PG11+: no rewrite)
	exec(`DROP TABLE scratch`)                                                                           // data loss
	exec(`DROP INDEX orders_total_idx`)
	exec(`CREATE TABLE audit (id int PRIMARY KEY)`) // harmless

	after, err := Take(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	rep := Diff(before, after, DiffOptions{})
	got := map[string]Finding{}
	for _, f := range rep.Findings {
		got[f.ID+" "+f.Object] = f
	}
	want := map[string]string{
		"table_rewritten public.orders":                                       SevWarning,
		"index_created_on_existing_table public.users_email_idx":              SevWarning,
		"constraint_validated_on_existing_table public.orders.orders_user_fk": SevWarning,
		"not_null_added public.users.name":                                    SevWarning,
		"column_dropped public.users.legacy":                                  SevCritical,
		"column_renamed public.users.email_address":                           SevWarning,
		"table_dropped public.scratch":                                        SevCritical,
		"index_dropped public.orders_total_idx":                               SevWarning,
		"column_type_changed public.orders.total":                             SevWarning,
		"table_created public.audit":                                          SevInfo,
		"column_added public.users.plan":                                      SevInfo,
	}
	for k, sev := range want {
		f, ok := got[k]
		if !ok {
			t.Errorf("missing finding %q; got %v", k, keys(got))
			continue
		}
		if f.Severity != sev {
			t.Errorf("%s: severity %s, want %s", k, f.Severity, sev)
		}
	}
	// ADD COLUMN ... DEFAULT 'free' (a constant) must NOT be reported as a rewrite of users.
	if _, ok := got["table_rewritten public.users"]; ok {
		t.Errorf("constant-default ADD COLUMN reported as a rewrite")
	}
	if !rep.Dangerous || rep.Summary.Critical != 2 {
		t.Errorf("summary %+v dangerous=%v", rep.Summary, rep.Dangerous)
	}

	// The same database compared with itself has an identical structure.
	if cmp := Compare(after, after, "a", "b"); !cmp.Identical {
		t.Errorf("self-compare not identical: %+v", cmp.Differences)
	}
	cmp := Compare(before, after, "before", "after")
	if cmp.Identical {
		t.Fatal("before/after compare reported identical")
	}
	var sawScratch bool
	for _, d := range cmp.Differences {
		if d.Object == "public.scratch" && d.Field == "relation" && d.B == "" {
			sawScratch = true
		}
	}
	if !sawScratch {
		t.Errorf("compare did not report the dropped table: %+v", cmp.Differences)
	}
}

func keys(m map[string]Finding) string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return strings.Join(out, "; ")
}
