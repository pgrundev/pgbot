package findings

import (
	"regexp"
	"testing"

	"github.com/pgrundev/pgbot/internal/model"
)

func warmWindow() model.Window {
	age := int64(45 * 24 * 3600) // 45 days — well past the cold-window threshold
	return model.Window{WindowAgeSeconds: &age}
}

func guardByID(f *model.Finding, id string) *model.SafetyGuard {
	if f == nil || f.Safety == nil {
		return nil
	}
	for i := range f.Safety.BlockingCaveats {
		if f.Safety.BlockingCaveats[i].ID == id {
			return &f.Safety.BlockingCaveats[i]
		}
	}
	return nil
}

// TestSafety_perDestructiveFinding is model-free: each destructive finding must
// carry its structured guard, keyed on a stable ID (never text), with the right
// Kind/Action and a Verify present iff it's a precondition.
func TestSafety_perDestructiveFinding(t *testing.T) {
	cases := []struct {
		name      string
		ctx       *model.Context
		findingID string
		guardID   string
		action    string
		kind      string
	}{
		{"unused_indexes",
			&model.Context{Window: warmWindow(), Indexes: &model.Indexes{Unused: []model.IndexStat{{Schema: "public", Table: "t", Name: "i", Bytes: 50 << 20, Method: "btree", Columns: []string{"a"}}}}},
			"unused_indexes", "unused_index.per_node", model.ActionDropIndex, model.GuardPrecondition},
		{"crdb_unused_indexes",
			&model.Context{Server: model.ServerInfo{Engine: "cockroachdb"}, Indexes: &model.Indexes{Section: model.Section{Exactness: model.ExactnessScraped}, Unused: []model.IndexStat{{Schema: "public", Table: "t", Name: "i", UnusedForSeconds: 8 * 24 * 3600}}}},
			"crdb_unused_indexes", "crdb_unused_index.observation_window", model.ActionDropIndex, model.GuardPrecondition},
		{"redundant_indexes",
			&model.Context{Indexes: &model.Indexes{Redundant: []model.RedundantIndex{{Schema: "public", Table: "t", Name: "i", CoveredBy: "j", Bytes: 1 << 20}}}},
			"redundant_indexes", "redundant_index.covering_equivalent", model.ActionDropIndex, model.GuardPrecondition},
		{"index_invalid",
			&model.Context{Schema: &model.SchemaFingerprint{Objects: []model.SchemaObject{{Kind: "index", Identity: "public.t.i", Invalid: true}}}},
			"index_invalid", "invalid_index.no_build_running", model.ActionDropIndex, model.GuardPrecondition},
		{"checksum_failures",
			&model.Context{Checksums: &model.Checksums{Failures: []model.ChecksumFailure{{Database: "d", Count: 2}}}},
			"checksum_failures", "checksum.no_vacuum_full", model.ActionVacuumFull, model.GuardProhibition},
		{"txid_wraparound",
			&model.Context{Limits: &model.Limits{MaxXIDAge: xidWraparoundWarn + 1}},
			"txid_wraparound", "wraparound.no_vacuum_full", model.ActionVacuumFull, model.GuardProhibition},
		{"mxid_wraparound",
			&model.Context{Limits: &model.Limits{MaxMXIDAge: xidWraparoundWarn + 1}},
			"mxid_wraparound", "mxid_wraparound.no_vacuum_full", model.ActionVacuumFull, model.GuardProhibition},
		{"sequence_exhaustion",
			&model.Context{Sequences: &model.Sequences{Items: []model.SequenceUsage{{Schema: "public", Name: "s", LastValue: 2_000_000_000, Ceiling: 2_147_483_647, PctUsed: 0.93}}}},
			"sequence_exhaustion", "narrow_column.table_rewrite", model.ActionAlterColumnType, model.GuardPrecondition},
		{"int4_identity_column",
			&model.Context{Sequences: &model.Sequences{NarrowIdentity: []model.NarrowIdentityColumn{{Schema: "public", Table: "t", Column: "id", Type: "int4"}}}},
			"int4_identity_column", "narrow_column.table_rewrite", model.ActionAlterColumnType, model.GuardPrecondition},
		{"replication_slot_inactive",
			&model.Context{Replication: &model.Replication{Slots: []model.ReplicationSlot{{Name: "s", Type: "logical", Active: false, RetainedBytes: 2 << 30}}}},
			"replication_slot_inactive", "replication_slot.live_consumer", model.ActionDropReplicationSlot, model.GuardProhibition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := has(Compute(tc.ctx), tc.findingID)
			if f == nil {
				t.Fatalf("finding %s did not fire from its fixture", tc.findingID)
			}
			g := guardByID(f, tc.guardID)
			if g == nil {
				t.Fatalf("finding %s is missing safety guard %q; Safety=%+v", tc.findingID, tc.guardID, f.Safety)
			}
			if g.Action != tc.action {
				t.Errorf("guard action = %q, want %q", g.Action, tc.action)
			}
			if g.Kind != tc.kind {
				t.Errorf("guard kind = %q, want %q", g.Kind, tc.kind)
			}
			if tc.kind == model.GuardPrecondition && g.Verify == nil {
				t.Errorf("a precondition guard must state what clears it (Verify)")
			}
			if tc.kind == model.GuardProhibition && g.Verify != nil {
				t.Errorf("a prohibition guard must have nil Verify (nothing clears it while the state holds)")
			}
		})
	}
}

// destructiveRemediationRE matches a remediation that tells the user to run a
// destructive or irreversible statement. Kept to the verbs that actually appear in
// pgbot remediations to avoid flagging innocuous text.
var destructiveRemediationRE = regexp.MustCompile(
	`(?i)\bDROP\s+INDEX\b|\bVACUUM\s+FULL\b|\bREINDEX\b|\bDROP\s+REPLICATION\s+SLOT\b|pg_drop_replication_slot|\bALTER\b[^.]{0,40}\bTYPE\b|\bDROP\s+TABLE\b|\bDROP\s+COLUMN\b|\bTRUNCATE\b`)

// TestSafety_regressionGuard (Step 8) is the backstop: any finding whose
// remediation suggests a destructive action MUST declare a Safety guard. It runs
// over a context that triggers every destructive finding, so deleting a guard — or
// adding a new destructive finding to this fixture without one — fails the build.
func TestSafety_regressionGuard(t *testing.T) {
	c := &model.Context{
		Window: warmWindow(),
		Indexes: &model.Indexes{
			Unused:    []model.IndexStat{{Schema: "public", Table: "t", Name: "ui", Bytes: 50 << 20, Method: "btree", Columns: []string{"a"}}},
			Redundant: []model.RedundantIndex{{Schema: "public", Table: "t", Name: "ri", CoveredBy: "j", Bytes: 1 << 20}},
		},
		Schema:    &model.SchemaFingerprint{Objects: []model.SchemaObject{{Kind: "index", Identity: "public.t.bad", Invalid: true}}},
		Checksums: &model.Checksums{Failures: []model.ChecksumFailure{{Database: "d", Count: 3}}},
		Limits:    &model.Limits{MaxXIDAge: xidWraparoundWarn + 1, MaxMXIDAge: xidWraparoundWarn + 1},
		Sequences: &model.Sequences{
			Items:          []model.SequenceUsage{{Schema: "public", Name: "s", LastValue: 2_000_000_000, Ceiling: 2_147_483_647, PctUsed: 0.93}},
			NarrowIdentity: []model.NarrowIdentityColumn{{Schema: "public", Table: "t", Column: "id", Type: "int4"}},
		},
		Replication: &model.Replication{Slots: []model.ReplicationSlot{{Name: "s", Type: "logical", Active: false, RetainedBytes: 2 << 30}}},
	}
	destructive := 0
	for _, f := range Compute(c) {
		if !destructiveRemediationRE.MatchString(f.Remediation) {
			continue
		}
		destructive++
		if f.Safety == nil || len(f.Safety.BlockingCaveats) == 0 {
			t.Errorf("finding %q has a destructive remediation but declares no Safety guard.\n  remediation: %q", f.ID, f.Remediation)
		}
	}
	// Guard against the guard silently covering nothing (e.g. the fixture stops
	// triggering the destructive findings).
	if destructive < 6 {
		t.Fatalf("expected ≥6 destructive-remediation findings in the fixture, saw %d — extend the fixture so the regression check has teeth", destructive)
	}
}
