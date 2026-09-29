package collect_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pgrundev/pgbot/internal/collect"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/findings"
	"github.com/pgrundev/pgbot/internal/model"
)

// A publication over a table with no primary key is accepted by Postgres and only
// rejects the first UPDATE. Build exactly that, prove the write really fails, then
// run the real collector as the read-only role and check the finding. Adding a
// primary key must clear it.
func TestIntegration_replicaIdentityMissing(t *testing.T) {
	su := os.Getenv("PGBOT_TEST_SUPERUSER_DSN")
	if su == "" {
		t.Skip("set PGBOT_TEST_SUPERUSER_DSN (a superuser DSN) to run the publication fixture")
	}
	ro := dsn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, su)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })

	cleanup := func() {
		_, _ = admin.Exec(context.Background(), `DROP PUBLICATION IF EXISTS pgbot_it_pub`)
		_, _ = admin.Exec(context.Background(), `DROP TABLE IF EXISTS public.pgbot_it_nopk`)
	}
	cleanup()
	t.Cleanup(cleanup)
	if _, err := admin.Exec(ctx, `
		CREATE TABLE public.pgbot_it_nopk (id bigint, note text);
		INSERT INTO public.pgbot_it_nopk VALUES (1, 'a');
		CREATE PUBLICATION pgbot_it_pub FOR TABLE public.pgbot_it_nopk`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	// The breakage this finding predicts, confirmed against the server.
	if _, err := admin.Exec(ctx, `UPDATE public.pgbot_it_nopk SET note = 'b'`); err == nil {
		t.Fatal("expected the UPDATE to fail without a replica identity")
	}

	target, err := conn.Connect(ctx, ro)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer target.Close()
	run := func() *model.Context {
		c, err := collect.Run(ctx, target, collect.Options{Interval: 200 * time.Millisecond, ASHHz: 0})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if c.ReplicaIdentity == nil || c.ReplicaIdentity.Exactness != model.ExactnessScraped {
			t.Fatalf("the section must be collected by the read-only role, got %+v", c.ReplicaIdentity)
		}
		return c
	}

	c := run()
	var row *model.PublishedTable
	for i := range c.ReplicaIdentity.Unidentifiable {
		if c.ReplicaIdentity.Unidentifiable[i].Name == "pgbot_it_nopk" {
			row = &c.ReplicaIdentity.Unidentifiable[i]
		}
	}
	if row == nil {
		t.Fatalf("the published table must be collected, got %+v", c.ReplicaIdentity.Unidentifiable)
	}
	if row.Identity != "d" || row.Publications != "pgbot_it_pub" {
		t.Errorf("collected row must mirror the catalog: %+v", *row)
	}
	var f *model.Finding
	for _, x := range findings.Compute(c) {
		if x.ID == "replica_identity_missing" {
			f = &x
			break
		}
	}
	if f == nil || f.Severity != model.SeverityCritical {
		t.Fatalf("expected critical replica_identity_missing, got %+v", f)
	}

	if _, err := admin.Exec(ctx, `ALTER TABLE public.pgbot_it_nopk ADD PRIMARY KEY (id)`); err != nil {
		t.Fatalf("add pk: %v", err)
	}
	for _, r := range run().ReplicaIdentity.Unidentifiable {
		if r.Name == "pgbot_it_nopk" {
			t.Fatalf("a primary key must clear the finding, still reported: %+v", r)
		}
	}
	// And the write the finding predicted now succeeds.
	if _, err := admin.Exec(ctx, `UPDATE public.pgbot_it_nopk SET note = 'c'`); err != nil {
		t.Errorf("UPDATE should succeed once a replica identity exists: %v", err)
	}
}
