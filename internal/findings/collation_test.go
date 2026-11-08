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

	unused := func(name string) model.CollationMismatch {
		return model.CollationMismatch{Kind: "collation", Name: name, Provider: "icu", Recorded: "153.14", Actual: "153.120", Unreferenced: true}
	}

	// #130: an ICU upgrade under a libc database drifts every built-in
	// pg_catalog ICU collation at once. Nothing uses them, so no index can be
	// out of order — one info line, not a warn listing hundreds of collations.
	onlyUnused := &model.Context{
		Server: model.ServerInfo{Database: "app"},
		Collation: &model.Collation{Mismatches: []model.CollationMismatch{
			unused("pg_catalog.af-x-icu"), unused("pg_catalog.am-x-icu"), unused("pg_catalog.ar-x-icu"),
			unused("pg_catalog.as-x-icu"), unused("pg_catalog.az-x-icu"),
		}},
	}
	f = has(Compute(onlyUnused), "collation_version_mismatch")
	if f == nil || f.Severity != model.SeverityInfo {
		t.Fatalf("only unreferenced collations drifting must be info, got %+v", f)
	}
	if len(f.Evidence) != 1 || !strings.Contains(f.Evidence[0], "5 ") ||
		!strings.Contains(f.Evidence[0], "pg_catalog.af-x-icu") || strings.Contains(f.Evidence[0], "pg_catalog.az-x-icu") {
		t.Errorf("unreferenced collations must collapse to one counted, truncated line: %v", f.Evidence)
	}
	if !strings.Contains(f.Remediation, "REFRESH VERSION") || strings.Contains(f.Title, "corrupt") {
		t.Errorf("info case must point at REFRESH VERSION and not read as corruption: %q / %q", f.Title, f.Remediation)
	}

	mixed := &model.Context{
		Server: model.ServerInfo{Database: "app"},
		Collation: &model.Collation{Mismatches: []model.CollationMismatch{
			{Kind: "collation", Name: "public.de_phonebook", Provider: "icu", Recorded: "153.14", Actual: "153.120"},
			unused("pg_catalog.af-x-icu"), unused("pg_catalog.am-x-icu"),
		}},
	}
	f = has(Compute(mixed), "collation_version_mismatch")
	if f == nil || f.Severity != model.SeverityWarn {
		t.Fatalf("a referenced collation drifting must still warn alongside unused ones, got %+v", f)
	}
	if !strings.HasPrefix(f.Title, "1 ") || len(f.Evidence) != 2 || !strings.Contains(f.Evidence[0], "public.de_phonebook") {
		t.Errorf("warn must count and list only the referenced collation, unused ones as one line: %q %v", f.Title, f.Evidence)
	}

	dbAndUnused := &model.Context{
		Server: model.ServerInfo{Database: "app"},
		Collation: &model.Collation{Mismatches: []model.CollationMismatch{
			{Kind: "database", Name: "app", Provider: "libc", Recorded: "2.31", Actual: "2.36"},
			unused("pg_catalog.af-x-icu"),
		}},
	}
	if f := has(Compute(dbAndUnused), "collation_version_mismatch"); f == nil || f.Severity != model.SeverityCritical {
		t.Errorf("a drifted database default stays critical whatever else drifted, got %+v", f)
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
