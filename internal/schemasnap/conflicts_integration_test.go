package schemasnap

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pggo"
)

// Three branches of one base database, each migrated on its own copy (a
// CREATE DATABASE … TEMPLATE clone, as pgx and PGRun branches are), checked
// against each other. Runs when PGBOT_TEST_SUPERUSER_DSN is set.
func TestConflictsAgainstRealBranches(t *testing.T) {
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
	cfg, err := pggo.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Format("150405")
	open := func(db string) *pggo.Conn {
		t.Helper()
		c := cfg.Copy()
		c.Database = db
		conn, err := pggo.ConnectConfig(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	run := func(c *pggo.Conn, sqls ...string) {
		t.Helper()
		for _, s := range sqls {
			if _, err := c.Exec(ctx, s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	}
	createDB := func(name, template string) {
		t.Helper()
		q := "CREATE DATABASE " + name
		if template != "" {
			q += " TEMPLATE " + template
		}
		run(admin, q)
		t.Cleanup(func() { admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)") })
	}

	base := "pgbot_conf_base_" + stamp
	createDB(base, "")
	bc := open(base)
	run(bc,
		`CREATE TABLE users (id bigserial PRIMARY KEY, email text, name text)`,
		`INSERT INTO users (email, name) SELECT 'u'||g||'@x', 'n'||g FROM generate_series(1, 2000) g`,
		`CREATE TABLE orders (id bigserial PRIMARY KEY, user_id bigint REFERENCES users (id), total int)`,
		`INSERT INTO orders (user_id, total) SELECT (g % 2000) + 1, g FROM generate_series(1, 2000) g`,
		`CREATE INDEX orders_total_idx ON orders (total)`,
		`CREATE TABLE coupons (id bigserial PRIMARY KEY, code text)`,
	)
	before, err := Take(ctx, bc)
	if err != nil {
		t.Fatal(err)
	}
	bc.Close() // a template must have no other connections

	branch := func(label string, migration ...string) Changeset {
		t.Helper()
		name := "pgbot_conf_" + strings.TrimPrefix(label, "#") + "_" + stamp
		createDB(name, base)
		c := open(name)
		defer c.Close()
		run(c, migration...)
		after, err := Take(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		return Changes(before, after, ChangesetMeta{Label: label})
	}

	pr1 := branch("#1",
		`ALTER TABLE users RENAME COLUMN email TO email_address`,
		`ALTER TABLE users ADD COLUMN plan text NOT NULL DEFAULT 'free'`,
		`CREATE TABLE invoices (id bigserial PRIMARY KEY, order_id bigint REFERENCES orders (id), coupon_id bigint REFERENCES coupons (id))`,
	)
	pr2 := branch("#2",
		`ALTER TABLE users ALTER COLUMN email TYPE varchar(320)`,
		`CREATE INDEX users_email_idx ON users (email)`,
		`CREATE TABLE invoices (id bigserial PRIMARY KEY, amount numeric)`,
		`DROP INDEX orders_total_idx`,
	)
	pr3 := branch("#3",
		`ALTER TABLE orders ADD COLUMN discount int`,
		`DROP TABLE coupons`,
	)
	pr4 := branch("#4", `SELECT 1`) // touches no schema

	// The rename is recognised as a rename (same catalog lineage), not a drop + add.
	if !hasChange(pr1, OpRenamed, KindColumn, "public.users.email_address") {
		t.Errorf("#1 rename not detected: %+v", pr1.Changes)
	}
	if hasChange(pr1, OpDropped, KindColumn, "public.users.email") {
		t.Errorf("#1 rename reported as a drop: %+v", pr1.Changes)
	}
	if len(pr4.Changes) != 0 {
		t.Errorf("#4 changed nothing, got %+v", pr4.Changes)
	}

	rep := Conflicts([]*Changeset{&pr1, &pr2, &pr3, &pr4}, "")
	got := map[string]Conflict{}
	for _, it := range rep.Items {
		got[it.Severity+" "+it.A+" "+it.B+" "+it.Object] = it
	}
	want := []string{
		// #1 renames users.email; #2 changes its type and indexes it.
		"conflict #1 #2 public.users.email",
		// both create invoices, differently
		"conflict #1 #2 public.invoices",
		// #1's new invoices table references coupons; #3 drops coupons
		"conflict #1 #3 public.coupons",
		// #2 drops an index on orders, #3 adds a column to orders
		"overlap #2 #3 public.orders",
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("missing %q\ngot: %s", k, itemKeys(rep.Items))
		}
	}
	if len(rep.Items) != len(want) {
		t.Errorf("got %d items, want %d: %s", len(rep.Items), len(want), itemKeys(rep.Items))
	}
	if it := got["conflict #1 #2 public.users.email"]; len(it.BChanges) != 2 {
		t.Errorf("users.email conflict should carry #2's type change and index: %+v", it.BChanges)
	}

	// Focused on #3: only its own pairs.
	f := Conflicts([]*Changeset{&pr1, &pr2, &pr3, &pr4}, "#3")
	for _, it := range f.Items {
		if it.A != "#3" {
			t.Errorf("focused report has a pair without #3 as A: %+v", it)
		}
	}
	if f.Summary.Conflicts != 1 || f.Summary.Overlaps != 1 {
		t.Errorf("focused #3 summary %+v, want 1 conflict + 1 overlap: %s", f.Summary, itemKeys(f.Items))
	}
	// The branch that changes nothing collides with nobody.
	if n := Conflicts([]*Changeset{&pr1, &pr2, &pr3, &pr4}, "#4"); len(n.Items) != 0 {
		t.Errorf("#4 has conflicts: %s", itemKeys(n.Items))
	}
}

func hasChange(s Changeset, op, kind, object string) bool {
	for _, c := range s.Changes {
		if c.Op == op && c.Kind == kind && c.Object == object {
			return true
		}
	}
	return false
}

func itemKeys(items []Conflict) string {
	var out []string
	for _, it := range items {
		out = append(out, it.Severity+" "+it.A+" "+it.B+" "+it.Object)
	}
	return strings.Join(out, "; ")
}
