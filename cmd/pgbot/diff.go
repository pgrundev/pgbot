package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/pgrundev/pgbot/internal/diff"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/render"
	"github.com/pgrundev/pgbot/internal/store"
	"github.com/spf13/cobra"
)

type diffFlags struct {
	since       time.Duration
	fingerprint string
	storePath   string
	noColor     bool
	json        bool
}

func newDiffCmd() *cobra.Command {
	var f diffFlags
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Compare the latest baseline snapshot against an earlier one (offline)",
		Long: "Diffs the most recent stored snapshot against the one nearest --since ago, from\n" +
			"the local baseline store — no connection needed. It prints the interval it\n" +
			"actually compared (not the one you asked for), and says up front when a stats\n" +
			"reset or pg_stat_statements eviction between the snapshots makes specific deltas\n" +
			"untrustworthy. It never compares two different databases.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDiff(cmd.OutOrStdout(), f)
		},
	}
	fl := cmd.Flags()
	fl.DurationVar(&f.since, "since", 24*time.Hour, "compare against the snapshot nearest this far back")
	fl.StringVar(&f.fingerprint, "fingerprint", "", "which database (fingerprint or a unique prefix); required if the store holds more than one")
	fl.StringVar(&f.storePath, "store", "", "baseline DB path (default: XDG state dir)")
	fl.BoolVar(&f.noColor, "no-color", false, "disable ANSI color")
	fl.BoolVar(&f.json, "json", false, "emit the diff as JSON")
	return cmd
}

// diffResult is the computed comparison, shared by `pgbot diff` and the MCP
// compare_to_baseline tool.
type diffResult struct {
	Item        store.ListItem
	Baseline    *store.Snapshot
	Current     *store.Snapshot
	Deltas      *model.Deltas
	ResetReason string
	PgssEvicted bool
	Requested   time.Duration
	Actual      time.Duration
}

// resolveDiff opens the store, resolves the target database, and computes the
// comparison of its latest snapshot against the one nearest `since` ago. The
// interval-honesty and reset/eviction detection live here so every consumer gets
// them (the agent needs the caveats more than a human does).
func resolveDiff(storePath, fpSpec string, since time.Duration) (*diffResult, error) {
	st, err := store.Open(storePath)
	if err != nil {
		return nil, fmt.Errorf("open baseline store: %w", err)
	}
	defer st.Close()

	items, err := st.List()
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("no baselines yet — run `pgbot inspect` first")
	}
	item, err := resolveFingerprint(items, fpSpec)
	if err != nil {
		return nil, err
	}
	current, err := st.Previous(item.Fingerprint, time.Now().UTC(), 0)
	if err != nil || current == nil {
		return nil, fmt.Errorf("no snapshot for %s", item.Fingerprint)
	}
	baseline, err := st.Previous(item.Fingerprint, current.CollectedAt, since)
	if err != nil {
		return nil, err
	}
	if baseline == nil || baseline.ID == current.ID {
		return nil, fmt.Errorf("not enough history: no snapshot at least %s before the latest (%s). "+
			"This database's oldest snapshot is %s back — try a smaller --since",
			render.HumanDur(since), current.CollectedAt.Format("2006-01-02 15:04"),
			render.HumanDur(current.CollectedAt.Sub(item.Oldest)))
	}
	// Both snapshots come from one resolved fingerprint, so a cross-database diff
	// (the P0-1 collision) is structurally impossible here.
	return &diffResult{
		Item:        item,
		Baseline:    baseline,
		Current:     current,
		Deltas:      diff.Compute(current.Context, &diff.Baseline{CollectedAt: baseline.CollectedAt, Context: baseline.Context}, nil),
		ResetReason: diff.StatsResetBetween(baseline.Context, current.Context),
		PgssEvicted: pgssEvictedBetween(baseline.Context, current.Context),
		Requested:   since,
		Actual:      current.CollectedAt.Sub(baseline.CollectedAt),
	}, nil
}

func runDiff(w io.Writer, f diffFlags) error {
	r, err := resolveDiff(f.storePath, f.fingerprint, f.since)
	if err != nil {
		return err
	}
	if f.json {
		return json.NewEncoder(os.Stdout).Encode(diffJSON(r))
	}
	return render.DiffReport(w, render.DiffInput{
		Color: useColor(f.noColor), Database: r.Item.Database, Fingerprint: r.Item.Fingerprint,
		BaselineAt: r.Baseline.CollectedAt, CurrentAt: r.Current.CollectedAt,
		Requested: r.Requested, Actual: r.Actual,
		ResetReason: r.ResetReason, PgssEvicted: r.PgssEvicted, Deltas: r.Deltas,
	})
}

// diffJSON is the machine-readable shape of a diff, shared by --json and the MCP
// tool — including the interval honesty and the reset/eviction caveats.
func diffJSON(r *diffResult) map[string]any {
	return map[string]any{
		"database":            r.Item.Database,
		"fingerprint":         r.Item.Fingerprint,
		"baseline_at":         r.Baseline.CollectedAt,
		"current_at":          r.Current.CollectedAt,
		"requested_seconds":   int64(r.Requested.Seconds()),
		"actual_seconds":      int64(r.Actual.Seconds()),
		"stats_reset_between": r.ResetReason,
		"pgss_evicted":        r.PgssEvicted,
		"changes":             deltasOrEmpty(r.Deltas),
	}
}

// resolveFingerprint picks the target database. With one in the store it's
// unambiguous; with several a fingerprint (or a unique prefix, or the database
// name) must be given, and an ambiguous or unknown one is a clear error — never a
// silent pick.
func resolveFingerprint(items []store.ListItem, spec string) (store.ListItem, error) {
	if spec == "" {
		if len(items) == 1 {
			return items[0], nil
		}
		return store.ListItem{}, fmt.Errorf("the store holds %d databases — pass --fingerprint:\n%s", len(items), listDatabases(items))
	}
	var matches []store.ListItem
	for _, it := range items {
		if it.Fingerprint == spec || strings.HasPrefix(it.Fingerprint, spec) || it.Database == spec {
			matches = append(matches, it)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return store.ListItem{}, fmt.Errorf("no database matches %q:\n%s", spec, listDatabases(items))
	default:
		return store.ListItem{}, fmt.Errorf("%q is ambiguous (%d matches) — use a longer prefix", spec, len(matches))
	}
}

// listDatabases renders the pick-one listing. Six databases can all be named
// "postgres" (snapshots deliberately store no host), so the line carries the
// rest of the identity the store has: server version, provider, and recency.
func listDatabases(items []store.ListItem) string {
	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "  %-14s %s (%d snapshots", it.Fingerprint[:min(14, len(it.Fingerprint))], it.Database, it.Count)
		if !it.Newest.IsZero() {
			fmt.Fprintf(&b, ", last %s", agoStr(&it.Newest))
		}
		b.WriteString(")")
		if it.Version != "" {
			fmt.Fprintf(&b, " · postgres %s", it.Version)
		}
		if it.Provider != "" && it.Provider != "unknown" {
			fmt.Fprintf(&b, " · %s", it.Provider)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// pgssEvictedBetween reports whether pg_stat_statements evicted entries between
// the two snapshots (dealloc rose), which makes per-query deltas incomplete.
func pgssEvictedBetween(base, cur *model.Context) bool {
	if base == nil || cur == nil || base.Queries == nil || cur.Queries == nil {
		return false
	}
	return cur.Queries.PgssDealloc > base.Queries.PgssDealloc
}

func deltasOrEmpty(d *model.Deltas) []model.Delta {
	if d == nil {
		return []model.Delta{}
	}
	return d.Changes
}
