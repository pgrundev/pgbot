package main

import (
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/findings"
	"github.com/pgrundev/pgbot/internal/model"
)

func TestComposeTargets(t *testing.T) {
	// No instances: one target per database, as --all-databases always did.
	got := composeTargets(nil, []string{"app", "billing"})
	if len(got) != 2 || got[0].instance != nil || got[0].database != "app" || got[1].database != "billing" {
		t.Fatalf("database-only plan = %+v", got)
	}
	// --all-instances alone: every member at the connection's own database.
	inst := []conn.AuroraInstance{{ID: "w", Writer: true, Host: "w.x.us-east-1.rds.amazonaws.com"}, {ID: "r1", Host: "r1.x.us-east-1.rds.amazonaws.com"}}
	got = composeTargets(inst, []string{""})
	if len(got) != 2 || got[0].instance.ID != "w" || got[0].database != "" || got[1].instance.ID != "r1" {
		t.Fatalf("instance-only plan = %+v", got)
	}
	// Both: the cross product, one member at a time, writer first.
	got = composeTargets(inst, []string{"app", "billing"})
	want := []string{"w/app", "w/billing", "r1/app", "r1/billing"}
	if len(got) != len(want) {
		t.Fatalf("cross plan has %d targets, want %d", len(got), len(want))
	}
	for i, w := range want {
		if key := got[i].instance.ID + "/" + got[i].database; key != w {
			t.Errorf("target %d = %s, want %s", i, key, w)
		}
	}
	// Each target keeps its own member: a shared loop variable would alias them.
	if got[0].instance == got[2].instance {
		t.Error("targets share one AuroraInstance pointer across members")
	}
	if !strings.Contains(got[0].label(), "instance w (writer)") || !strings.Contains(got[0].label(), "app") {
		t.Errorf("label = %q", got[0].label())
	}
}

func TestPartialCoverageExit(t *testing.T) {
	if partialCoverageExit(exitClean) != exitFailure || partialCoverageExit(exitCritical) != exitFailure {
		t.Error("partial coverage must fail the run even when the findings are clean")
	}
	if partialCoverageExit(exitUsage) != exitUsage {
		t.Error("a higher code must not be lowered")
	}
}

func instanceContext(id, role, db string, findingIDs ...string) *model.Context {
	c := &model.Context{}
	c.Server.Instance, c.Server.InstanceRole, c.Server.Database = id, role, db
	for _, f := range findingIDs {
		c.Findings = append(c.Findings, model.Finding{ID: f})
	}
	return c
}

// Cluster-wide findings are reduced to one per server. Under --all-instances a
// member is a server: the same finding on two members survives on both, while a
// second database on the same member drops it.
func TestDedupeClusterWide_perInstance(t *testing.T) {
	cw := clusterWideFindingID(t)
	contexts := []*model.Context{
		instanceContext("w", "writer", "app", cw),
		instanceContext("w", "writer", "billing", cw),
		instanceContext("r1", "reader", "app", cw),
	}
	dedupeClusterWide(contexts)
	if len(contexts[0].Findings) != 1 || len(contexts[1].Findings) != 0 || len(contexts[2].Findings) != 1 {
		t.Fatalf("findings per context after dedupe = %d/%d/%d; want 1/0/1", len(contexts[0].Findings), len(contexts[1].Findings), len(contexts[2].Findings))
	}
	if !contexts[0].Findings[0].ClusterScoped || !contexts[2].Findings[0].ClusterScoped {
		t.Error("surviving copies must be marked cluster-scoped")
	}
}

func TestMergeContexts_tagsByInstance(t *testing.T) {
	cw := clusterWideFindingID(t)
	contexts := []*model.Context{
		instanceContext("w", "writer", "app", "table_bloat", cw),
		instanceContext("r1", "reader", "app", "table_bloat", cw),
	}
	contexts[0].Findings[0].Object = "public.orders"
	contexts[1].Findings[0].Object = "public.orders"
	dedupeClusterWide(contexts)
	merged := mergeContexts(contexts)
	var objects []string
	for _, f := range merged.Findings {
		objects = append(objects, f.ID+"="+f.Object)
	}
	joined := strings.Join(objects, " ")
	for _, want := range []string{"table_bloat=instance:w/db:app/public.orders", "table_bloat=instance:r1/db:app/public.orders", cw + "=instance:w", cw + "=instance:r1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("merged objects %q lack %q", joined, want)
		}
	}
}

func TestContextHeaderAndScope(t *testing.T) {
	plain := &model.Context{}
	plain.Server.Database = "app"
	if contextHeader(plain) != "database: app" || contextScope(plain) != "app" {
		t.Errorf("plain header/scope = %q / %q", contextHeader(plain), contextScope(plain))
	}
	member := instanceContext("prod-1", "writer", "app")
	if contextHeader(member) != "instance: prod-1 (writer) · database: app" || contextScope(member) != "prod-1/app" {
		t.Errorf("member header/scope = %q / %q", contextHeader(member), contextScope(member))
	}
}

// clusterWideFindingID picks any finding the catalog marks cluster-wide, so the
// tests don't hard-code one that might be reclassified.
func clusterWideFindingID(t *testing.T) string {
	t.Helper()
	for _, id := range []string{"checksum_failures", "connection_saturation", "wraparound_risk", "archiver_failing", "replication_lag"} {
		if findings.ClusterWide(id) {
			return id
		}
	}
	t.Skip("no known cluster-wide finding id available")
	return ""
}
