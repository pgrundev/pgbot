package findings

import (
	"testing"

	"github.com/pgrundev/pgbot/internal/model"
)

func TestStableObject_rejectsEphemeral(t *testing.T) {
	valid := []string{
		"", // cluster-scoped
		"setting:track_io_timing",
		"slot:wal2json_prod",
		"sub:orders_sync",
		"q:8213498112345",
		"db:analytics",
		"public.issues",
		"public.index_issues_on_last_seen_at",
	}
	for _, s := range valid {
		if !StableObject(s) {
			t.Errorf("StableObject(%q) = false, want true", s)
		}
	}
	// Ephemeral identifiers a suppression rule must never key on.
	invalid := []string{
		"42",             // pid
		"18446744073",    // xid/oid
		"0/1A2B3C4",      // LSN-ish (no dot, has slash)
		"setting:",       // empty payload
		"slot:",          // empty payload
		"orders",         // unqualified relation
		"12345.678",      // all-digit "relation" → could be a pid.oid
	}
	for _, s := range invalid {
		if StableObject(s) {
			t.Errorf("StableObject(%q) = true, want false", s)
		}
	}
}

// Every finding Compute can emit must carry a suppression-stable Object. We drive
// a Context rich enough to trip the object-bearing findings (settings, an inactive
// slot, a down subscription) alongside the cluster/summary ones, and assert the
// invariant on all of them — no finding may expose a PID-like Object (B2-0 DoD 8).
func TestCompute_everyFindingHasStableObject(t *testing.T) {
	contexts := []*model.Context{
		{
			Window:   model.Window{StatsWindowDays: ptr(120), SampleSeconds: 1},
			Health:   &model.Health{CacheHitRatio: ptr(0.80), CacheBlocks: i64(50_000), RollbackRatio: ptr(0.15), TPS: ptr(200)},
			Activity: &model.Activity{IdleInTransaction: 2, LongestXactSec: 400},
			Locks:    &model.Locks{BlockedCount: 1, Chains: []model.BlockingRow{{BlockedPID: 42, WaitSeconds: 30}}},
			Indexes:  &model.Indexes{Unused: []model.IndexStat{{Schema: "public", Table: "orders", Name: "big_idx", Bytes: 5 << 20}}},
			Tables:   &model.Tables{Top: []model.TableStat{{Schema: "public", Name: "orders", DeadRatio: 0.35, LiveTuples: 90000, DeadTuples: 48000, ModsSinceAnalyze: 5000}}},
			Queries:  &model.Queries{Enabled: true},
		},
		{ // settings-driven object findings
			Settings: &model.Settings{Params: map[string]string{
				"fsync": "off", "full_page_writes": "off", "autovacuum": "off",
				"statement_timeout": "0", "track_io_timing": "off",
			}},
		},
		{ // per-object loop findings: slot + subscription
			Replication: &model.Replication{
				Slots:         []model.ReplicationSlot{{Name: "wal2json_prod", Active: false, RetainedBytes: 3 << 30, Type: "logical"}},
				Subscriptions: []model.Subscription{{Name: "orders_sync", WorkerRunning: false}},
			},
		},
	}
	seen := 0
	for _, c := range contexts {
		for _, f := range Compute(c) {
			seen++
			if !StableObject(f.Object) {
				t.Errorf("finding %q emitted unstable Object %q", f.ID, f.Object)
			}
			// Every emitted ID must be in the knownIDs whitelist the config layer
			// validates against — otherwise a real finding can't be suppressed.
			if !KnownID(f.ID) {
				t.Errorf("finding %q emitted but missing from knownIDs (add it, or config can't reference it)", f.ID)
			}
		}
	}
	if seen == 0 {
		t.Fatal("test fixtures produced no findings — invariant never exercised")
	}
}
