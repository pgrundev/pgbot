package findings

import (
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/model"
)

func TestCollationVersionMismatch(t *testing.T) {
	dbDrift := &model.Context{
		Server: model.ServerInfo{Database: "app"},
		Collation: &model.Collation{Mismatches: []model.CollationMismatch{
			{Kind: "database", Name: "app", Provider: "libc", Recorded: "2.31", Actual: "2.36"},
		}},
	}
	f := has(Compute(dbDrift), "collation_version_mismatch")
	if f == nil || f.Severity != model.SeverityCritical {
		t.Fatalf("a drifted database default must fire critical, got %+v", f)
	}
	if f.Object != "db:app" {
		t.Errorf("object must be the database, for suppression keying; got %q", f.Object)
	}
	if len(f.Caveats) == 0 || !strings.Contains(f.Caveats[0], "AFTER reindexing") {
		t.Errorf("must carry the reindex-before-refresh caveat: %v", f.Caveats)
	}
	if len(f.Evidence) != 1 || !strings.Contains(f.Evidence[0], "2.31") || !strings.Contains(f.Evidence[0], "2.36") {
		t.Errorf("evidence must name both versions: %v", f.Evidence)
	}

	named := &model.Context{
		Server: model.ServerInfo{Database: "app"},
		Collation: &model.Collation{Mismatches: []model.CollationMismatch{
			{Kind: "collation", Name: "public.de_phonebook", Provider: "icu", Recorded: "153.14", Actual: "153.120"},
		}},
	}
	if f := has(Compute(named), "collation_version_mismatch"); f == nil || f.Severity != model.SeverityWarn {
		t.Errorf("a named collation drifting must fire warn, got %+v", f)
	}

	for _, healthy := range []*model.Context{
		{Collation: &model.Collation{}},
		{Collation: &model.Collation{Section: model.Section{Exactness: model.ExactnessUnavailable}}},
		{},
	} {
		if has(Compute(healthy), "collation_version_mismatch") != nil {
			t.Errorf("no mismatches must not fire: %+v", healthy.Collation)
		}
	}
}
