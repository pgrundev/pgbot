package collect

import (
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
)

func TestAssembleCockroachJobsPreservesOperationalDetailAndScrubsText(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.FixedZone("offset", -4*60*60))
	last := now.Add(-5 * time.Minute)
	highWater := now.Add(-10 * time.Second)
	c := &model.Context{Server: model.ServerInfo{Engine: "cockroachdb"}}
	assembleCockroachOperations(c, conn.Capabilities{Engine: conn.EngineCockroachDB, HasCRDBJobs: true}, sampled{A: cockroachSample{
		Jobs: []cockroachJobRow{{
			JobID: "42", JobType: "SCHEMA CHANGE", State: "running", CreatedAt: now.Add(-time.Hour),
			Progress: .625, ProgressKnown: true, Operation: "CREATE INDEX idx ON app.public.orders (email) WHERE tenant_id = ‹customer-secret›",
			StatusMessage: "backfilling span ‹private-key›", Error: `retry for id 987 key "dir_30/file_97.txt" hash a6ae387c3837363aaf3b17bd6bda93df40979abc and token ‹secret-token›`,
			LastUpdatedAt: &last, HighWaterAt: &highWater, TotalJobs: 40,
		}},
	}})
	h := c.Health.Cockroach
	if h.Jobs.Exactness != model.ExactnessScraped || h.JobsTotal != 40 || !h.JobsBounded || len(h.JobItems) != 1 {
		t.Fatalf("jobs=%+v", h)
	}
	j := h.JobItems[0]
	if !j.ProgressKnown || j.Progress != .625 || j.LastUpdatedAt == nil || j.LastUpdatedAt.Location() != time.UTC || j.HighWaterAt == nil {
		t.Fatalf("job detail=%+v", j)
	}
	for _, secret := range []string{"customer-secret", "private-key", "secret-token", "dir_30", "file_97", "a6ae387", "987", "‹", "›"} {
		if strings.Contains(j.Operation+j.StatusMessage+j.Error, secret) {
			t.Fatalf("job text leaked %q: %+v", secret, j)
		}
	}
	if !strings.Contains(j.Operation, "app.public.orders") {
		t.Fatalf("redaction lost schema object identity: %q", j.Operation)
	}
}

func TestCockroachJobsSQLUsesStableRedactedSurface(t *testing.T) {
	for _, want := range []string{
		"information_schema.crdb_jobs_with_progress",
		"crdb_internal.redactable_sql_constants",
		"crdb_internal.redact",
		"last_updated",
		"hlc_to_timestamp(resolved)",
		"count(*) OVER ()",
	} {
		if !strings.Contains(sqlCockroachJobs, want) {
			t.Errorf("job query missing %q", want)
		}
	}
}
