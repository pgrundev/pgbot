package ai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/model"
)

func sampleContext() *model.Context {
	age := int64(6 * 3600)
	return &model.Context{
		Server: model.ServerInfo{Database: "production", VersionText: "PostgreSQL 17.4"},
		Window: model.Window{WindowAgeSeconds: &age},
		Findings: []model.Finding{{
			ID: "unused_indexes", Severity: model.SeverityWarn, Title: "4 unused index(es) · 43.0 GiB",
			Remediation: "Drop with DROP INDEX CONCURRENTLY after confirming the caveats.",
			Impact:      model.Impact{Score: 80, Dimension: model.DimStorage, Estimate: "≈43.0 GiB reclaimable"},
			Confidence:  0.4,
			Caveats:     []string{"replication is active — these scan counts are from THIS node only"},
		}},
		WaitProfile: &model.WaitProfile{Available: true, Samples: 50, Buckets: []model.WaitBucket{
			{Type: "Lock", Share: 0.61, Events: []model.WaitEvent{{Event: "transactionid", Share: 0.4}, {Event: "tuple", Share: 0.21}}},
			{Type: "CPU", Share: 0.39},
		}},
		Events: []model.Event{{Kind: "config.changed", Object: "work_mem", Confidence: 0.5}},
	}
}

func TestBuildExplainPrompt_carriesFindingsAndCaveats(t *testing.T) {
	system, user := BuildExplainPrompt(sampleContext())

	// The system prompt must contain the load-bearing constraints.
	for _, want := range []string{"Do NOT invent", "caveats", "confidence", "EXECUTES a user's query"} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt missing constraint %q", want)
		}
	}

	// The user payload must be valid JSON carrying the finding and its caveat, so
	// the model actually receives the safety clause.
	jsonStart := strings.Index(user, "{")
	if jsonStart < 0 {
		t.Fatal("no JSON payload in user prompt")
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(user[jsonStart:]), &p); err != nil {
		t.Fatalf("user payload is not valid JSON: %v", err)
	}
	if !strings.Contains(user, "replication is active") {
		t.Error("caveat must be present in the payload sent to the model")
	}
	if !strings.Contains(user, "unused_indexes") {
		t.Error("finding id must be present in the payload")
	}
}

func TestBuildExplainPrompt_trimsWaitDetail(t *testing.T) {
	// Buckets are capped and each bucket's events trimmed to the top one so the
	// prompt stays tight.
	_, user := BuildExplainPrompt(sampleContext())
	// The Lock bucket had two events; only the top (transactionid) should ship.
	if strings.Count(user, "\"event\": \"tuple\"") != 0 {
		t.Error("secondary wait events should be trimmed from the prompt")
	}
	if !strings.Contains(user, "transactionid") {
		t.Error("the top wait event should be present")
	}
}

// The payload must not carry raw/literal query text. Our model never sets it,
// but assert the curated payload has no field that could smuggle it.
func TestBuildExplainPrompt_noRawQueryText(t *testing.T) {
	c := sampleContext()
	// Even if a QueryWaits carried sample text, it is the NORMALIZED form ($1);
	// assert we never emit a literal-looking value we didn't put there.
	_, user := BuildExplainPrompt(c)
	if strings.Contains(user, "raw_query") || strings.Contains(user, "password") {
		t.Error("payload must never contain raw query text or secrets")
	}
}

func TestBuildExplainPromptCockroachDB(t *testing.T) {
	qps := 13_800.0
	c := &model.Context{
		Server: model.ServerInfo{Engine: "cockroachdb", Database: "defaultdb", VersionText: "CockroachDB CCL v26.4.0"},
		Health: &model.Health{Cockroach: &model.CockroachHealth{
			AdminAPI: model.Section{Exactness: model.ExactnessScraped}, Prometheus: model.Section{Exactness: model.ExactnessSampled},
			NodesTotal: 6, NodesLive: 6, StoresTotal: 24, CapacityBytes: 8 << 40, AvailableBytes: 3 << 40,
			MaxStoreUsedRatio: .602, MaxCPUPercent: 66.8, SQLConnections: 713, QueriesPerSec: &qps,
			Nodes:        []model.CockroachNodeHealth{{NodeID: 1, Locality: "secret-locality", SQLConnections: 713}},
			Distribution: model.CockroachDistribution{Section: model.Section{Exactness: model.ExactnessScraped}, LiveStores: 24, ComparableStores: 24, ReplicaMin: 1600, ReplicaMax: 1750},
			Storage:      model.CockroachStorage{Section: model.Section{Exactness: model.ExactnessSampled}, MVCCGarbageBytes: 1 << 40, UninitializedReplicas: 2200, RaftCommandsPending: 47},
		}},
		Activity: &model.Activity{Section: model.Section{Exactness: model.ExactnessScraped}, Total: 713, Active: 42},
		Queries:  &model.Queries{Section: model.Section{Exactness: model.ExactnessScraped}, StatsSource: "activity_cache", WindowHours: 24, Top: []model.QueryStat{{Query: "SELECT secret_live_query"}}},
		Cockroach: &model.CockroachDB{
			LiveQueries: model.CockroachLiveQueries{Items: []model.CockroachLiveQuery{{User: "secret-user", AppName: "secret-app", Query: "SELECT secret_live_query"}}},
			Contention:  model.CockroachContention{Section: model.Section{Exactness: model.ExactnessScraped}, WindowMinutes: 60, TotalEvents: 12, TotalWaitMS: 3456},
		},
	}

	system, user := BuildExplainPrompt(c)
	combined := system + user
	for _, want := range []string{"This report is for CockroachDB", "PostgreSQL-only", `"engine": "cockroachdb"`, `"nodes_live": 6`, `"uninitialized_replicas": 2200`, `"contention_events": 12`} {
		if !strings.Contains(combined, want) {
			t.Errorf("CockroachDB prompt missing %q", want)
		}
	}
	for _, forbidden := range []string{"senior PostgreSQL expert", "secret-locality", "secret-user", "secret-app", "secret_live_query"} {
		if strings.Contains(combined, forbidden) {
			t.Errorf("CockroachDB prompt leaked or retained %q", forbidden)
		}
	}
}
