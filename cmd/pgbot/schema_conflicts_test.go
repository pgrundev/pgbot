package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/schemasnap"
)

func writeChangeset(t *testing.T, path string, s schemasnap.Changeset) {
	t.Helper()
	s.Kind, s.Version = schemasnap.ChangesetKind, schemasnap.ChangesetVersion
	b, _ := json.Marshal(s)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadChangesetsWalksDirsKeepsNewestPerLabelSkipsStrays(t *testing.T) {
	dir := t.TempDir()
	col := schemasnap.Change{Op: "created", Kind: "column", Object: "public.t.c", Table: "public.t", Detail: "d"}
	writeChangeset(t, filepath.Join(dir, "mine.json"), schemasnap.Changeset{Label: "#1", After: "2026-01-01T00:00:00Z", Changes: []schemasnap.Change{col}})
	writeChangeset(t, filepath.Join(dir, "others", "pr-2", "old.json"), schemasnap.Changeset{Label: "#2", After: "2026-01-01T00:00:00Z"})
	writeChangeset(t, filepath.Join(dir, "others", "pr-2b", "new.json"), schemasnap.Changeset{Label: "#2", After: "2026-02-01T00:00:00Z", Changes: []schemasnap.Change{col}})
	os.WriteFile(filepath.Join(dir, "others", "stray.json"), []byte(`{"kind":"something-else"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "others", "notes.txt"), []byte(`x`), 0o644)

	sets, err := loadChangesets([]string{filepath.Join(dir, "mine.json"), filepath.Join(dir, "others")})
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 2 || sets[1].Label != "#2" || len(sets[1].Changes) != 1 {
		t.Fatalf("got %d sets: %+v", len(sets), sets)
	}
	// A named file that is not a changeset is an error, not a silent skip.
	if _, err := loadChangesets([]string{filepath.Join(dir, "others", "stray.json")}); err == nil {
		t.Error("stray file named explicitly: want an error")
	}
}

func sampleReport() schemasnap.ConflictReport {
	a := &schemasnap.Changeset{Label: "#12", Changes: []schemasnap.Change{
		{Op: "altered", Kind: "column", Object: "public.users.email", Table: "public.users", Detail: "email | NOT NULL added"}}}
	b := &schemasnap.Changeset{Label: "#11", Title: "Rename <email>", Ref: "abcdef123456", Changes: []schemasnap.Change{
		{Op: "renamed", Kind: "column", Object: "public.users.email_address", From: "public.users.email", Table: "public.users", Detail: "renamed"}}}
	return schemasnap.Conflicts([]*schemasnap.Changeset{a, b}, "#12")
}

func TestConflictsMarkdownIsACommentWeCanFindAgain(t *testing.T) {
	var buf bytes.Buffer
	writeConflictsMarkdown(&buf, sampleReport())
	out := buf.String()
	if !strings.HasPrefix(out, ConflictsCommentMarker+"\n") {
		t.Fatalf("marker missing: %q", out[:40])
	}
	for _, want := range []string{"1 conflict(s)", "This PR (#12)", "#11 (Rename &lt;email&gt;)", `email \| NOT NULL added`, "#11 @ abcdef1"} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown missing %q:\n%s", want, out)
		}
	}
}

func TestConflictsMarkdownClean(t *testing.T) {
	a := &schemasnap.Changeset{Label: "#1"}
	var buf bytes.Buffer
	writeConflictsMarkdown(&buf, schemasnap.Conflicts([]*schemasnap.Changeset{a}, "#1"))
	if !strings.Contains(buf.String(), "Database conflicts: none") || !strings.Contains(buf.String(), "change no schema objects") {
		t.Errorf("clean report: %s", buf.String())
	}
}

func TestConflictsAnnotationsEscapeWorkflowCommands(t *testing.T) {
	var buf bytes.Buffer
	writeConflictsAnnotations(&buf, sampleReport())
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "::error title=Database conflict with #11 on public.users.email::") {
		t.Fatalf("got %q", buf.String())
	}
	if !strings.Contains(lines[0], "%0A") {
		t.Errorf("newlines not escaped: %q", lines[0])
	}
}

func TestConflictsExitCode(t *testing.T) {
	rep := sampleReport()
	if conflictsExitCode(rep, "none") != 0 || conflictsExitCode(rep, "conflict") != 2 || conflictsExitCode(rep, "overlap") != 2 {
		t.Error("conflict exit codes")
	}
	ov := schemasnap.ConflictReport{Summary: schemasnap.ConflictSummary{Overlaps: 1}}
	if conflictsExitCode(ov, "conflict") != 0 || conflictsExitCode(ov, "overlap") != 1 {
		t.Error("overlap exit codes")
	}
}

func TestCompareMarkdownSummarisesBySide(t *testing.T) {
	rep := schemasnap.CompareReport{ALabel: "Golden Copy", BLabel: "production", Differences: []schemasnap.Difference{
		{Object: "public.t", Field: "relation", A: "", B: "table"},
		{Object: "public.t2.c", Field: "column", A: "", B: "c int"},
		{Object: "public.t2.d", Field: "column", A: "", B: "d int"},
		{Object: "public.old_idx", Field: "index", A: "CREATE INDEX old_idx …", B: ""},
		{Object: "public.t2.e", Field: "column_definition", A: "e int", B: "e bigint"},
	}}
	var buf bytes.Buffer
	writeCompareMarkdown(&buf, rep)
	out := buf.String()
	for _, want := range []string{
		"Golden Copy differs from production: 5 structural difference(s)",
		"**Missing on Golden Copy** (present on production): 1 table, 2 columns",
		"**Only on Golden Copy** (gone from production): 1 index",
		"**Defined differently**: 1 column",
		"| `public.t2.e` | column definition | e int | e bigint |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	buf.Reset()
	writeCompareMarkdown(&buf, schemasnap.CompareReport{ALabel: "Golden Copy", BLabel: "production", Identical: true})
	if !strings.Contains(buf.String(), "### Golden Copy matches production") {
		t.Errorf("identical: %s", buf.String())
	}
}
