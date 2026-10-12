package collect_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/collect"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/findings"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pggo"
)

// Issue #11: a CREATE INDEX CONCURRENTLY that fails during the build (here: a
// unique build over duplicate keys — deterministic) leaves indisvalid = false,
// indisready = false, indislive = true and a 0-byte relation. PostgreSQL does
// not maintain such an index on writes, so pgbot must not report it as critical
// write overhead. This runs the real fingerprint SQL against a real failed build
// and checks both the collected catalog state and the resulting finding.
func TestIntegration_invalidIndexDebrisIsNotCritical(t *testing.T) {
	su := os.Getenv("PGBOT_TEST_SUPERUSER_DSN")
	if su == "" {
		t.Skip("set PGBOT_TEST_SUPERUSER_DSN (a superuser DSN) to run the failed-CIC fixture")
	}
	ro := dsn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	admin, err := pggo.Connect(ctx, su)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	if _, err := admin.SimpleQuery(ctx, `
		DROP TABLE IF EXISTS public.pgbot_it_dup;
		CREATE TABLE public.pgbot_it_dup (id serial primary key, v int);
		INSERT INTO public.pgbot_it_dup (v) SELECT g % 100 FROM generate_series(1, 20000) g`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), `DROP TABLE IF EXISTS public.pgbot_it_dup`) })
	// Must fail (duplicate keys) — and leave the invalid index behind. CIC cannot
	// run inside a transaction block; a bare Exec is autocommit.
	if _, err := admin.Exec(ctx, `CREATE UNIQUE INDEX CONCURRENTLY pgbot_it_dup_v_uidx ON public.pgbot_it_dup (v)`); err == nil {
		t.Fatal("expected the unique concurrent build to fail on duplicates")
	}
	var valid, ready, live bool
	var bytes int64
	if err := admin.QueryRow(ctx, `SELECT indisvalid, indisready, indislive, pg_relation_size(indexrelid) FROM pg_index WHERE indexrelid = 'public.pgbot_it_dup_v_uidx'::regclass`).
		Scan(&valid, &ready, &live, &bytes); err != nil {
		t.Fatalf("the failed build should have left an index row: %v", err)
	}
	if valid || ready || !live {
		t.Fatalf("fixture precondition: expected indisvalid=false indisready=false indislive=true, got %v/%v/%v", valid, ready, live)
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
	if c.Schema == nil {
		t.Fatal("schema fingerprint missing")
	}
	var obj *model.SchemaObject
	for i := range c.Schema.Objects {
		o := &c.Schema.Objects[i]
		if o.Kind == "index" && strings.HasSuffix(o.Identity, ".pgbot_it_dup_v_uidx") {
			obj = o
			break
		}
	}
	if obj == nil {
		t.Fatal("the invalid index must appear in the schema fingerprint")
	}
	if !obj.Invalid || obj.IndexReady || !obj.IndexLive || obj.Bytes != bytes {
		t.Fatalf("collected state must mirror pg_index: got invalid=%v ready=%v live=%v bytes=%d (catalog bytes=%d)",
			obj.Invalid, obj.IndexReady, obj.IndexLive, obj.Bytes, bytes)
	}

	var f *model.Finding
	for _, x := range findings.Compute(c) {
		if x.ID == "index_invalid" {
			f = &x
			break
		}
	}
	if f == nil {
		t.Fatal("index_invalid must fire (the debris still needs cleanup)")
	}
	if f.Severity != model.SeverityWarn {
		t.Errorf("failed-build debris must be warn, got %s: %+v", f.Severity, f)
	}
	if strings.Contains(strings.ToLower(f.Detail), "maintained on every write") {
		t.Errorf("must not claim write maintenance for indisready=false: %q", f.Detail)
	}
	if len(f.Evidence) == 0 || !strings.Contains(f.Evidence[0], "indisready = false") {
		t.Errorf("evidence should carry the catalog state: %v", f.Evidence)
	}
}
