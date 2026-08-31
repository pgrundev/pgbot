package collect

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
)

func TestIntegrationCockroachActivity(t *testing.T) {
	dsn := os.Getenv("PGBOT_TEST_CRDB_DSN")
	if dsn == "" {
		t.Skip("set PGBOT_TEST_CRDB_DSN to run the CockroachDB integration test")
	}
	target, err := conn.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer target.Close()
	if !target.Caps.IsCockroachDB() {
		t.Fatalf("engine = %q, want cockroachdb", target.Caps.EngineName())
	}

	c, err := Run(context.Background(), target, Options{ASHHz: 0})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if c.Server.Engine != "cockroachdb" || c.Activity == nil || c.Activity.Exactness != model.ExactnessScraped {
		t.Fatalf("CockroachDB activity not collected: server=%+v activity=%+v", c.Server, c.Activity)
	}
	if c.Health == nil || c.Health.Exactness != model.ExactnessUnavailable {
		t.Fatalf("PostgreSQL health collector should be explicitly unavailable: %+v", c.Health)
	}
	if target.Caps.HasCRDBStmtStats {
		if c.Queries == nil || !c.Queries.Enabled || c.Queries.Exactness != model.ExactnessCumulative {
			t.Fatalf("CockroachDB statement statistics not collected: %+v", c.Queries)
		}
		if target.Caps.HasCRDBStmtActivity {
			if c.Queries.StatsSource != cockroachStatsSourceActivityCache || !c.Queries.Bounded || c.Queries.WindowHours != 24 {
				t.Fatalf("cached query-statistics coverage is mislabeled: %+v", c.Queries)
			}
		} else if c.Queries.StatsSource != cockroachStatsSourcePublic || c.Queries.Bounded || c.Queries.WindowHours != 1 {
			t.Fatalf("public query-statistics fallback is mislabeled: %+v", c.Queries)
		}
	} else if c.Queries == nil || c.Queries.Exactness != model.ExactnessUnavailable {
		t.Fatalf("missing statement-statistics view should be explicit: %+v", c.Queries)
	}
	if c.Cockroach == nil || c.Cockroach.LiveQueries.Exactness != model.ExactnessScraped {
		t.Fatalf("CockroachDB live queries not collected: %+v", c.Cockroach)
	}
	if target.Caps.HasCRDBContention && c.Cockroach.Contention.Exactness != model.ExactnessScraped {
		t.Fatalf("CockroachDB contention events not collected: %+v", c.Cockroach.Contention)
	}
	if target.Caps.HasCRDBInsights && c.Cockroach.ExecutionInsights.Exactness != model.ExactnessCumulative {
		t.Fatalf("CockroachDB execution insights not collected: %+v", c.Cockroach.ExecutionInsights)
	}
	if c.WAL == nil || c.WAL.Reason != errUnsupportedOnCockroach.Error() {
		t.Fatalf("PostgreSQL WAL collector should be explicitly unavailable: %+v", c.WAL)
	}
	if c.WaitProfile == nil || c.WaitProfile.Reason != model.WaitSamplerUnsupportedCockroachReason {
		t.Fatalf("CockroachDB wait sampling reason is misleading: %+v", c.WaitProfile)
	}
}

func TestIntegrationCockroachLiveQuery(t *testing.T) {
	dsn := os.Getenv("PGBOT_TEST_CRDB_DSN")
	if dsn == "" {
		t.Skip("set PGBOT_TEST_CRDB_DSN to run the CockroachDB integration test")
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	workload, err := pgx.Connect(context.Background(), dsn+sep+"application_name=crdb_integration_workload")
	if err != nil {
		t.Fatalf("connect workload: %v", err)
	}
	defer workload.Close(context.Background())

	queryCtx, cancelQuery := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := workload.Exec(queryCtx, "SELECT pg_sleep(5)")
		done <- err
	}()
	defer func() {
		cancelQuery()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}()

	target, err := conn.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect pgbot: %v", err)
	}
	defer target.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, runErr := Run(context.Background(), target, Options{ASHHz: 0})
		if runErr != nil {
			t.Fatalf("collect: %v", runErr)
		}
		if c.Cockroach != nil {
			for _, q := range c.Cockroach.LiveQueries.Items {
				if q.AppName == "crdb_integration_workload" && strings.Contains(q.Query, "pg_sleep") {
					if c.Activity == nil || c.Activity.Active < 1 {
						t.Fatalf("live query was visible but activity did not count it: %+v", c.Activity)
					}
					return
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("running pg_sleep query never appeared in CockroachDB live queries")
}
