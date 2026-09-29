package collect_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/collect"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/correlate"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pggo"
)

// TestIntegration_indexCorrelation exercises the new index attributes (method,
// columns) end-to-end against a real server, and the exclusions that must hold
// regardless of the stats window: a REPLICA IDENTITY index and a primary key are
// never "unused". It also confirms the confidence grades that are window-
// independent (redundant → catalog_proven; gin/expression/partial → inconclusive).
// The plain-btree → needs_code_check grade depends on a warm window, so it is only
// asserted when the window isn't cold.
func TestIntegration_indexCorrelation(t *testing.T) {
	d := os.Getenv("PGBOT_TEST_SUPERUSER_DSN")
	if d == "" {
		t.Skip("set PGBOT_TEST_SUPERUSER_DSN (a superuser DSN) to run the index-correlation test")
	}
	ctx := context.Background()
	admin, err := pggo.Connect(ctx, d)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer admin.Close()

	// A table big enough that its indexes cross pgbot's 16 KB collector floor, with
	// one of each shape the classifier distinguishes.
	if _, err := admin.SimpleQuery(ctx, `
		DROP TABLE IF EXISTS public.corr_job, public.corr_ri;
		CREATE TABLE public.corr_job (id bigint primary key, "externalIdNormalized" text, tags jsonb, status text, customer_id bigint);
		INSERT INTO public.corr_job
		  SELECT g, 'ext-'||g, '{"k":"v"}'::jsonb, (ARRAY['open','closed','pending'])[1+g%3], g%500
		  FROM generate_series(1, 40000) g;
		CREATE INDEX "corr_Job_externalIdNormalized_idx" ON public.corr_job("externalIdNormalized");
		CREATE INDEX corr_job_expr_idx    ON public.corr_job(lower(status));
		CREATE INDEX corr_job_partial_idx ON public.corr_job(customer_id) WHERE status = 'open';
		CREATE INDEX corr_job_tags_gin    ON public.corr_job USING gin(tags);
		CREATE INDEX corr_job_cust_status ON public.corr_job(customer_id, status);
		CREATE INDEX corr_job_cust_only   ON public.corr_job(customer_id);
		CREATE TABLE public.corr_ri (id int, u text NOT NULL);
		CREATE UNIQUE INDEX corr_ri_u_key ON public.corr_ri(u);
		ALTER TABLE public.corr_ri REPLICA IDENTITY USING INDEX corr_ri_u_key;`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	defer admin.Exec(context.Background(), `DROP TABLE IF EXISTS public.corr_job, public.corr_ri`)

	target, err := conn.Connect(ctx, d)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer target.Close()

	c, err := collect.Run(ctx, target, collect.Options{Interval: time.Second, ASHHz: 0})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if c.Indexes == nil {
		t.Fatal("no indexes section")
	}

	// The new attributes must be populated on the collected indexes.
	byName := map[string]model.IndexStat{}
	for _, s := range append(append([]model.IndexStat{}, c.Indexes.Unused...), c.Indexes.Largest...) {
		byName[s.Name] = s
	}
	if g := byName["corr_job_tags_gin"]; g.Method != "gin" {
		t.Errorf("gin index method = %q, want gin", g.Method)
	}
	if ext := byName["corr_Job_externalIdNormalized_idx"]; len(ext.Columns) != 1 || ext.Columns[0] != "externalIdNormalized" {
		t.Errorf("btree columns = %v, want [externalIdNormalized]; method=%q", ext.Columns, ext.Method)
	}

	// Window-independent exclusions: replica-identity and PK are never unused.
	for _, s := range c.Indexes.Unused {
		if s.Name == "corr_ri_u_key" {
			t.Error("a REPLICA IDENTITY index must never be reported as unused")
		}
		if s.Name == "corr_job_pkey" {
			t.Error("a primary-key index must never be reported as unused")
		}
	}

	rep := correlate.Build(c, nil)
	grade := map[string]correlate.Confidence{}
	for _, ix := range rep.Indexes {
		grade[ix.Index] = ix.Confidence
	}
	// Redundant (prefix) is catalog_proven regardless of window.
	if grade["corr_job_cust_only"] != correlate.CatalogProven {
		t.Errorf("prefix-redundant index should be catalog_proven, got %q", grade["corr_job_cust_only"])
	}
	// GIN / expression / partial are inconclusive regardless of window.
	for _, n := range []string{"corr_job_tags_gin", "corr_job_expr_idx", "corr_job_partial_idx"} {
		if grade[n] != correlate.Inconclusive {
			t.Errorf("%s should be inconclusive, got %q", n, grade[n])
		}
	}
	// Plain btree over bare columns is actionable only with a warm window.
	if !rep.ColdWindow {
		if grade["corr_Job_externalIdNormalized_idx"] != correlate.NeedsCodeCheck {
			t.Errorf("plain btree (warm window) should be needs_code_check, got %q", grade["corr_Job_externalIdNormalized_idx"])
		}
		for _, ix := range rep.Indexes {
			if ix.Index == "corr_Job_externalIdNormalized_idx" {
				if !contains(ix.SearchTerms, "external_id_normalized") || !contains(ix.SearchTerms, "EXTERNAL_ID_NORMALIZED") {
					t.Errorf("needs_code_check must emit all case variants: %v", ix.SearchTerms)
				}
			}
		}
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
