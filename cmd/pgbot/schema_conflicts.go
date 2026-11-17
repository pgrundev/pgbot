package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/schemasnap"
	"github.com/spf13/cobra"
)

// ConflictsCommentMarker opens every markdown report so a CI job can find and
// update its own earlier PR comment instead of adding a new one.
const ConflictsCommentMarker = "<!-- pgbot-schema-conflicts -->"

func newSchemaChangesetCmd() *cobra.Command {
	var meta schemasnap.ChangesetMeta
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "changeset <before> <after>",
		Short: "Every schema object a branch's migrations created, dropped, renamed or altered",
		Long: "Lists what changed between two schema states, for `pgbot schema conflicts`. Each side is a snapshot\n" +
			"file from `pgbot schema snapshot` or a connection string (snapshotted now, catalog only).\n" +
			"Take <before> on the target branch's schema and <after> once the branch's migrations ran on top of it.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			before, err := snapshotFrom(ctx, args[0])
			if err != nil {
				return err
			}
			after, err := snapshotFrom(ctx, args[1])
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), schemasnap.Changes(before, after, meta))
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&meta.Label, "label", "", "who made the changes, e.g. a PR number (#123) or a branch name")
	fl.StringVar(&meta.Title, "title", "", "free text shown next to the label, e.g. the PR title")
	fl.StringVar(&meta.Ref, "ref", "", "commit the changes were measured at")
	fl.StringVar(&meta.URL, "url", "", "link for the label, e.g. the PR URL")
	fl.DurationVar(&timeout, "timeout", 30*time.Second, "wall-clock budget for snapshotting a connection")
	return cmd
}

// snapshotFrom reads a snapshot file, or takes a snapshot when arg is a
// connection string rather than an existing file.
func snapshotFrom(ctx context.Context, arg string) (*schemasnap.Snapshot, error) {
	if _, err := os.Stat(arg); err == nil {
		return readSnapshot(arg)
	}
	if !strings.Contains(arg, "://") && !strings.Contains(arg, "=") {
		return nil, fmt.Errorf("%s: no such snapshot file (and not a connection string)", arg)
	}
	target, err := conn.Connect(ctx, arg)
	if err != nil {
		return nil, err
	}
	defer target.Close()
	return schemasnap.Take(ctx, target.Pool)
}

func newSchemaConflictsCmd() *cobra.Command {
	var format, focus, failOn string
	cmd := &cobra.Command{
		Use:   "conflicts <changeset.json|dir>...",
		Short: "Where branches' schema changes collide: the same object, or the same table (offline)",
		Long: "Checks changesets from `pgbot schema changeset` against each other. A conflict means merging both\n" +
			"branches breaks (both create one name, one drops or renames what the other changes); an overlap means\n" +
			"both change the same table in different ways. Directories are searched for changeset files.\n" +
			"With --focus, only pairs that include that changeset's label are checked.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if failOn != "none" && failOn != SevConflictFlag && failOn != SevOverlapFlag {
				return usageErrf("unknown --fail-on %q (valid: conflict, overlap, none)", failOn)
			}
			sets, err := loadChangesets(args)
			if err != nil {
				return err
			}
			if focus != "" && !hasLabel(sets, focus) {
				return fmt.Errorf("--focus %q: no changeset with that label", focus)
			}
			rep := schemasnap.Conflicts(sets, focus)
			w := cmd.OutOrStdout()
			switch format {
			case "text", "":
				writeConflictsText(w, rep)
			case "json":
				if err := writeJSON(w, rep); err != nil {
					return err
				}
			case "markdown":
				writeConflictsMarkdown(w, rep)
			case "github":
				writeConflictsAnnotations(w, rep)
			default:
				return usageErrf("unknown --format %q (valid: text, json, markdown, github)", format)
			}
			if code := conflictsExitCode(rep, failOn); code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&format, "format", "text", "output format: text|json|markdown (a PR comment)|github (workflow annotations)")
	fl.StringVar(&focus, "focus", "", "only check this changeset (by label) against the others")
	fl.StringVar(&failOn, "fail-on", "none", "exit 2 on a conflict, or 1 on an overlap too: conflict|overlap|none")
	return cmd
}

const (
	SevConflictFlag = schemasnap.SevConflict
	SevOverlapFlag  = schemasnap.SevOverlap
)

func conflictsExitCode(rep schemasnap.ConflictReport, failOn string) int {
	switch failOn {
	case SevConflictFlag:
		if rep.Summary.Conflicts > 0 {
			return 2
		}
	case SevOverlapFlag:
		if rep.Summary.Conflicts > 0 {
			return 2
		}
		if rep.Summary.Overlaps > 0 {
			return 1
		}
	}
	return 0
}

func hasLabel(sets []*schemasnap.Changeset, label string) bool {
	for _, s := range sets {
		if s.Label == label {
			return true
		}
	}
	return false
}

// loadChangesets reads the named files and every changeset under the named
// directories. A named file must be a changeset; a file found in a directory
// that is not one is skipped. Two changesets with one label (a branch
// recorded twice) keep the newer.
func loadChangesets(args []string) ([]*schemasnap.Changeset, error) {
	byLabel := map[string]*schemasnap.Changeset{}
	var order []string
	keep := func(s *schemasnap.Changeset, path string) {
		if s.Label == "" {
			s.Label = path
		}
		old, seen := byLabel[s.Label]
		if !seen {
			order = append(order, s.Label)
		}
		if !seen || s.After > old.After {
			byLabel[s.Label] = s
		}
	}
	for _, arg := range args {
		st, err := os.Stat(arg)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			s, err := readChangeset(arg)
			if err != nil {
				return nil, err
			}
			keep(s, arg)
			continue
		}
		err = filepath.WalkDir(arg, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
				return err
			}
			if s, err := readChangeset(path); err == nil {
				keep(s, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	out := make([]*schemasnap.Changeset, 0, len(order))
	for _, l := range order {
		out = append(out, byLabel[l])
	}
	return out, nil
}

var errNotChangeset = errors.New("not a schema changeset")

func readChangeset(path string) (*schemasnap.Changeset, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s schemasnap.Changeset
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w: %v", path, errNotChangeset, err)
	}
	if s.Kind != schemasnap.ChangesetKind {
		return nil, fmt.Errorf("%s: %w (kind %q)", path, errNotChangeset, s.Kind)
	}
	return &s, nil
}

func others(rep schemasnap.ConflictReport) []schemasnap.ChangesetRef {
	var out []schemasnap.ChangesetRef
	for _, c := range rep.Changesets {
		if c.Label != rep.Focus {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

func focusRef(rep schemasnap.ConflictReport) (schemasnap.ChangesetRef, bool) {
	for _, c := range rep.Changesets {
		if c.Label == rep.Focus {
			return c, true
		}
	}
	return schemasnap.ChangesetRef{}, false
}

func writeConflictsText(w io.Writer, rep schemasnap.ConflictReport) {
	if rep.Focus != "" {
		if f, ok := focusRef(rep); ok && f.Changes == 0 {
			fmt.Fprintf(w, "%s changes no schema objects; nothing to collide with\n", rep.Focus)
			return
		}
	}
	if len(rep.Items) == 0 {
		fmt.Fprintf(w, "no schema conflicts between %d changesets\n", len(rep.Changesets))
		return
	}
	fmt.Fprintf(w, "schema conflicts: %d conflict · %d overlap\n", rep.Summary.Conflicts, rep.Summary.Overlaps)
	for _, it := range rep.Items {
		fmt.Fprintf(w, "\n%-8s %s ↔ %s  %s\n  %s\n", it.Severity, it.A, it.B, it.Object, it.Reason)
		for _, d := range it.AChanges {
			fmt.Fprintf(w, "  %s: %s\n", it.A, d)
		}
		for _, d := range it.BChanges {
			fmt.Fprintf(w, "  %s: %s\n", it.B, d)
		}
	}
	fmt.Fprintf(w, "\n%s\n", rep.Caveat)
}

func mdCell(lines []string) string {
	r := strings.NewReplacer("|", `\|`, "\n", " ", "<", "&lt;", ">", "&gt;")
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = r.Replace(l)
	}
	return strings.Join(out, "<br>")
}

func mdLabel(c schemasnap.ChangesetRef) string {
	s := c.Label
	if c.URL != "" && !strings.HasPrefix(c.Label, "#") { // GitHub links "#123" by itself
		s = fmt.Sprintf("[%s](%s)", c.Label, c.URL)
	}
	if c.Title != "" {
		s += " (" + strings.NewReplacer("|", `\|`, "<", "&lt;", ">", "&gt;").Replace(c.Title) + ")"
	}
	return s
}

func shortRef(r string) string {
	if len(r) > 7 {
		return r[:7]
	}
	return r
}

// writeConflictsMarkdown renders the PR comment. It starts with
// ConflictsCommentMarker, so a job can update the comment it posted before.
func writeConflictsMarkdown(w io.Writer, rep schemasnap.ConflictReport) {
	fmt.Fprintln(w, ConflictsCommentMarker)
	refs := map[string]schemasnap.ChangesetRef{}
	for _, c := range rep.Changesets {
		refs[c.Label] = c
	}
	f, focused := focusRef(rep)
	switch {
	case focused && f.Changes == 0:
		fmt.Fprintf(w, "### Database conflicts: none\n\nThis PR's migrations change no schema objects.\n")
		return
	case len(rep.Items) == 0:
		fmt.Fprintf(w, "### Database conflicts: none\n\n")
		if focused {
			fmt.Fprintf(w, "This PR's %d schema change(s) touch nothing that the other open PRs change.\n", f.Changes)
		} else {
			fmt.Fprintf(w, "No two of the %d changesets touch the same schema objects.\n", len(rep.Changesets))
		}
	default:
		head := fmt.Sprintf("%d conflict(s)", rep.Summary.Conflicts)
		if rep.Summary.Overlaps > 0 {
			head += fmt.Sprintf(", %d overlap(s)", rep.Summary.Overlaps)
		}
		fmt.Fprintf(w, "### Database conflicts with other open PRs: %s\n\n", head)
		if rep.Summary.Conflicts > 0 {
			fmt.Fprintf(w, "A **conflict** breaks the second PR to merge: its migration fails or changes an object the other PR relies on. "+
				"An **overlap** means both PRs change the same table: whichever merges second must re-run its migrations on top of the first.\n\n")
		} else {
			fmt.Fprintf(w, "An **overlap** means both PRs change the same table: whichever merges second must re-run its migrations on top of the first.\n\n")
		}
		for _, it := range rep.Items {
			title := "Conflict"
			if it.Severity == schemasnap.SevOverlap {
				title = "Overlap"
			}
			other := refs[it.B]
			fmt.Fprintf(w, "**%s** with %s on `%s`\n", title, mdLabel(other), it.Object)
			fmt.Fprintf(w, "%s.\n\n", it.Reason)
			aName := it.A
			if focused {
				aName = "This PR (" + it.A + ")"
			}
			fmt.Fprintf(w, "| %s | %s |\n|---|---|\n| %s | %s |\n\n", mdCell([]string{aName}), mdCell([]string{it.B}), mdCell(it.AChanges), mdCell(it.BChanges))
		}
	}
	if o := others(rep); len(o) > 0 && focused {
		var parts []string
		for _, c := range o {
			p := c.Label
			if c.Ref != "" {
				p += " @ " + shortRef(c.Ref)
			}
			parts = append(parts, p)
		}
		fmt.Fprintf(w, "\n<sub>Checked against %d open PR(s) with recorded schema changes: %s. %s</sub>\n", len(o), strings.Join(parts, ", "), rep.Caveat)
	} else {
		fmt.Fprintf(w, "\n<sub>%s</sub>\n", rep.Caveat)
	}
}

// writeConflictsAnnotations prints GitHub Actions workflow commands, one per
// item: an error for a conflict, a warning for an overlap.
func writeConflictsAnnotations(w io.Writer, rep schemasnap.ConflictReport) {
	esc := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A")
	escProp := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C")
	for _, it := range rep.Items {
		level := "error"
		if it.Severity == schemasnap.SevOverlap {
			level = "warning"
		}
		title := fmt.Sprintf("Database %s with %s on %s", it.Severity, it.B, it.Object)
		msg := it.Reason + "\n" + it.A + ": " + strings.Join(it.AChanges, "; ") + "\n" + it.B + ": " + strings.Join(it.BChanges, "; ")
		fmt.Fprintf(w, "::%s title=%s::%s\n", level, escProp.Replace(title), esc.Replace(msg))
	}
}
