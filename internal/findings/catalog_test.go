package findings

import (
	"testing"

	"github.com/pgrundev/pgbot/internal/model"
)

// TestCatalog_matchesEmitted drives fixtures that trip each catalogued finding
// and asserts the code emits exactly the dimension, severity, and object class
// the Meta declares (B7 DoD 9). A doc page that disagrees with the binary is
// worse than no doc — this is the code-side half of that guard; the docs test is
// the other half. Catalogued findings the fixtures here don't trip are checked
// only structurally; extend the fixtures as the catalog grows in B7-1.
func TestCatalog_matchesEmitted(t *testing.T) {
	// One Context per catalogued exemplar, built to trip exactly that finding.
	fixtures := map[string]*model.Context{
		"sequence_exhaustion": {
			Sequences: &model.Sequences{Items: []model.SequenceUsage{
				{Schema: "public", Name: "orders_id_seq", PctUsed: 0.95, LastValue: 2000000000, Ceiling: 2147483647},
			}},
		},
		"low_hot_update_ratio": {
			Tables: &model.Tables{Top: []model.TableStat{
				{Schema: "public", Name: "issues", Updates: 500000, HotUpdates: 5000}, // 1% HOT
			}},
		},
		"checksum_failures": {
			Checksums: &model.Checksums{Failures: []model.ChecksumFailure{{Database: "app", Count: 3}}},
		},
		"collation_version_mismatch": {
			Server: model.ServerInfo{Database: "app"},
			Collation: &model.Collation{Mismatches: []model.CollationMismatch{
				{Kind: "database", Name: "app", Provider: "libc", Recorded: "2.31", Actual: "2.36"},
			}},
		},
		"pgaudit_silent": {
			Server:   model.ServerInfo{Extensions: []string{"pgaudit"}},
			Settings: &model.Settings{Params: map[string]string{"pgaudit.log": "none"}},
		},
		"pgaudit_logs_parameters": {
			Server:   model.ServerInfo{Extensions: []string{"pgaudit"}},
			Settings: &model.Settings{Params: map[string]string{"pgaudit.log": "all", "pgaudit.log_parameter": "on"}},
		},
		"pgaudit_double_logging": {
			Server:   model.ServerInfo{Extensions: []string{"pgaudit"}},
			Settings: &model.Settings{Params: map[string]string{"pgaudit.log": "all", "log_statement": "all"}},
		},
	}

	for id, meta := range catalog {
		c, ok := fixtures[id]
		if !ok {
			continue // structural-only until B7-1 adds a fixture
		}
		f := has(Compute(c), id)
		if f == nil {
			t.Errorf("catalog fixture for %q did not emit it", id)
			continue
		}
		if f.Impact.Dimension != meta.Dimension {
			t.Errorf("%s: catalog dimension %q != emitted %q", id, meta.Dimension, f.Impact.Dimension)
		}
		if got := FindingObjectClass(*f); got != meta.ObjectClass {
			t.Errorf("%s: catalog object class %q != emitted %q (object=%q objects=%v)", id, meta.ObjectClass, got, f.Object, f.Objects)
		}
		// Base severity must be reachable; a CriticalWhen escalation means the base
		// isn't critical, and vice versa.
		if meta.CriticalWhen == "" && meta.Severity != f.Severity {
			t.Errorf("%s: catalog severity %q != emitted %q", id, meta.Severity, f.Severity)
		}
	}
}

// TestCatalog_idsAreKnown guards against a catalog entry for a finding that
// doesn't exist.
func TestCatalog_idsAreKnown(t *testing.T) {
	for id := range catalog {
		if !KnownID(id) {
			t.Errorf("catalog has %q which is not a known finding id", id)
		}
	}
}

func TestObjectClass(t *testing.T) {
	cases := map[string]string{
		"":                        "cluster",
		"setting:track_io_timing": "setting",
		"slot:wal2json":           "slot",
		"sub:orders":              "sub",
		"q:123":                   "query",
		"db:analytics":            "db",
		"public.orders":           "relation",
	}
	for in, want := range cases {
		if got := ObjectClass(in); got != want {
			t.Errorf("ObjectClass(%q) = %q, want %q", in, got, want)
		}
	}
}
