package collect

import (
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
)

func TestCockroachQueryIDStableAndAppScoped(t *testing.T) {
	a := cockroachQueryID("deadbeef", "checkout")
	if a == 0 || a != cockroachQueryID("deadbeef", "checkout") {
		t.Fatal("CockroachDB query ID must be non-zero and stable")
	}
	if a == cockroachQueryID("deadbeef", "billing") {
		t.Fatal("the same fingerprint in different applications needs a distinct baseline identity")
	}
}

func TestAssembleCockroachQueriesPreservesSourceCoverage(t *testing.T) {
	c := &model.Context{}
	caps := conn.Capabilities{Engine: conn.EngineCockroachDB, HasViewActivity: true, HasCRDBStmtStats: true}
	s := sampled{A: cockroachQueriesSample{
		Rows:        []cockroachQueryRow{{Fingerprint: "abc", Calls: 2, TotalMS: 10, TotalExecAll: 10}},
		Source:      cockroachStatsSourceActivityCache,
		WindowHours: 24,
		Bounded:     true,
	}}
	assembleCockroachQueries(c, caps, s)
	if c.Queries == nil || c.Queries.StatsSource != cockroachStatsSourceActivityCache || c.Queries.WindowHours != 24 || !c.Queries.Bounded {
		t.Fatalf("query source coverage was lost: %+v", c.Queries)
	}
}

func TestCockroachLiveQueryAgeClampedToZero(t *testing.T) {
	c := &model.Context{}
	caps := conn.Capabilities{Engine: conn.EngineCockroachDB, HasViewActivity: true}
	s := sampled{A: cockroachSample{Live: []cockroachLiveRow{{QueryID: "q1", Query: "SELECT 1", AgeS: -0.25}}}}
	(cockroachCollector{}).Assemble(c, caps, s, 0*time.Second, Options{})
	if c.Cockroach == nil || len(c.Cockroach.LiveQueries.Items) != 1 || c.Cockroach.LiveQueries.Items[0].AgeSec != 0 {
		t.Fatalf("negative live-query age was not clamped: %+v", c.Cockroach)
	}
}
