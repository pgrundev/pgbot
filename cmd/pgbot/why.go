package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pgrundev/pgbot/internal/collect"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/render"
	"github.com/pgrundev/pgbot/internal/store"
	"github.com/pgrundev/pgbot/internal/why"
	"github.com/spf13/cobra"
)

type whyFlags struct {
	window      time.Duration
	fingerprint string
	storePath   string
	maxChains   int
	noColor     bool
	json        bool
	duration    time.Duration // >0 = also sample the live database
	hz          int
}

// newWhyCmd answers "why did this get slow?" from the local baseline store —
// fully offline, like diff: every `pgbot inspect` already stored a snapshot,
// and the why-engine connects the onsets across them into causal chains. No
// model generates anything; same contract as the findings engine.
func newWhyCmd() *cobra.Command {
	var f whyFlags
	cmd := &cobra.Command{
		Use:   "why [count]",
		Short: "Explain a regression from baseline history: symptom ← mechanism ← antecedent (offline)",
		Long: "Builds per-object time series from the stored snapshots, finds sustained\n" +
			"shifts, and connects them into causal chains — \"this query slowed 3.2×\n" +
			"because seq scans surged on orders after the table grew 18%\" — with the\n" +
			"numbers and onset times for every hop. Deterministic: the chains are computed\n" +
			"from Postgres's own counters across your history, never guessed. Runs offline\n" +
			"from the local store; each `pgbot inspect` adds one snapshot of history.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// `pgbot why 10` is the ergonomic form of --max-chains=10; with
			// --duration the same slot may instead carry a connection string.
			dsnArg := ""
			if len(args) == 1 {
				if whyArgIsDSN(args[0]) {
					dsnArg = args[0]
				} else {
					n, err := strconv.Atoi(args[0])
					if err != nil || n < 1 {
						return fmt.Errorf("the argument is how many chains to show, e.g. `pgbot why 10` — got %q", args[0])
					}
					f.maxChains = n
				}
			}
			if f.duration > 0 {
				return runWhyLive(cmd.Context(), cmd.OutOrStdout(), f, dsnArg)
			}
			if dsnArg != "" {
				return fmt.Errorf("a connection string only makes sense with --duration (offline why reads the local store)")
			}
			return runWhy(cmd.OutOrStdout(), f)
		},
	}
	fl := cmd.Flags()
	fl.DurationVar(&f.window, "window", 7*24*time.Hour, "how far back to analyze")
	fl.StringVar(&f.fingerprint, "fingerprint", "", "which database (fingerprint or a unique prefix); required if the store holds more than one")
	fl.StringVar(&f.storePath, "store", "", "baseline DB path (default: XDG state dir)")
	fl.IntVar(&f.maxChains, "max-chains", 5, "how many chains to report, worst first (or pass it as the argument: `pgbot why 10`)")
	fl.BoolVar(&f.noColor, "no-color", false, "disable ANSI color")
	fl.BoolVar(&f.json, "json", false, "emit the report as JSON (why_schema_version 1.1.0)")
	fl.DurationVar(&f.duration, "duration", 0, "also sample the LIVE database for this window (needs a connection) and diagnose where time went")
	fl.IntVar(&f.hz, "hz", 10, "live sampling rate with --duration (clamped to 1–20)")
	return cmd
}

// computeWhy opens the store, resolves the database, and runs the analysis —
// shared by the command and the MCP tool.
func computeWhy(storePath, fpSpec string, window time.Duration, maxChains int) (why.Report, error) {
	st, err := store.Open(storePath)
	if err != nil {
		return why.Report{}, fmt.Errorf("open baseline store: %w", err)
	}
	defer st.Close()

	items, err := st.List()
	if err != nil {
		return why.Report{}, err
	}
	if len(items) == 0 {
		return why.Report{}, fmt.Errorf("no baselines yet — run `pgbot inspect` a few times first; each run stores one snapshot of history")
	}
	item, err := resolveFingerprint(items, fpSpec)
	if err != nil {
		return why.Report{}, err
	}

	snaps, err := st.LoadRange(item.Fingerprint, time.Now().UTC().Add(-window))
	if err != nil {
		return why.Report{}, err
	}
	samples := make([]why.Sample, len(snaps))
	for i, s := range snaps {
		samples[i] = why.Sample{At: s.CollectedAt, C: s.Context}
	}
	events, err := st.RecentEvents(item.Fingerprint, 200)
	if err != nil { // events enrich antecedents; their absence must not block the analysis
		events = nil
	}
	report := why.Analyze(samples, events, why.Options{MaxChains: maxChains})
	// "Only 2 snapshots" while the listing said 26 reads as a bug: when the
	// store holds history the window cut off, say so and name the fix.
	if older := item.Count - len(samples); older > 0 && len(samples) < 3 {
		report.Notes = append(report.Notes, fmt.Sprintf(
			"the store holds %d more snapshot(s) for this database older than the %s window — widen it to reach them, e.g. --window %dh",
			older, window, int(window.Hours())*4))
	}
	return report, nil
}

func runWhy(w io.Writer, f whyFlags) error {
	report, err := computeWhy(f.storePath, f.fingerprint, f.window, f.maxChains)
	if err != nil {
		return err
	}

	if f.json {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	printWhy(w, report)
	return nil
}

// printWhy renders the narrative and the context a first-time reader needs:
// what was analyzed, how many regressions were found vs shown, one block per
// chain (symptom, then hops), the hedged confidence, and a how-to-read legend.
func printWhy(w io.Writer, r why.Report) {
	fmt.Fprintf(w, "why · %s · %d snapshots · %s → %s\n",
		r.Database, r.Snapshots, r.WindowStart.Format("Jan 2 15:04"), r.WindowEnd.Format("Jan 2 15:04"))
	for _, note := range r.Notes {
		fmt.Fprintf(w, "%s\n", note)
	}
	if r.Snapshots < 3 {
		return // the note above already says what to do
	}
	fmt.Fprintf(w, "analyzed %d quer%s and %d table%s from your stored history — found %d sustained regression%s",
		r.AnalyzedQueries, plural(r.AnalyzedQueries, "y", "ies"),
		r.AnalyzedTables, plural(r.AnalyzedTables, "", "s"),
		r.RegressionsFound, plural(r.RegressionsFound, "", "s"))
	if r.RegressionsFound > len(r.Chains) {
		fmt.Fprintf(w, " · showing the %d worst of %d (--max-chains for more)", len(r.Chains), r.RegressionsFound)
	}
	fmt.Fprint(w, "\n\n")
	if len(r.Chains) == 0 {
		fmt.Fprintln(w, "✓ nothing got measurably worse in the window — nothing to explain")
		return
	}
	for _, ch := range r.Chains {
		fmt.Fprintf(w, "● %s\n", ch.Symptom.Text)
		for _, h := range ch.Hops {
			fmt.Fprintf(w, "    ↳ %s\n", h.Text)
		}
		if ch.Confidence < 0.5 {
			fmt.Fprintf(w, "    possibly — confidence %.0f%%: no mechanism found in the stored history; the cause may be outside what pgbot collects\n", ch.Confidence*100)
		} else {
			fmt.Fprintf(w, "    confidence %.0f%% — computed from onset alignment across %d snapshots\n", ch.Confidence*100, r.Snapshots)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "how to read this: each block is one chain — what regressed ← the mechanism ← what set it off,")
	fmt.Fprintln(w, "with the numbers and the time each shift started, computed from your snapshot history.")
	fmt.Fprintln(w, "More history sharpens it: every `pgbot inspect` adds one snapshot.")
}

// plural picks the singular or plural suffix.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// whyArgIsDSN distinguishes a connection string from the chain-count argument:
// URLs and keyword/value DSNs are unmistakable, and a bare integer never is one.
func whyArgIsDSN(s string) bool {
	return strings.Contains(s, "://") || strings.Contains(s, "=")
}

// runWhyLive samples the live database (the waits study), classifies the
// result through the deterministic cause table, folds the counts into the
// store, and merges with the offline history analysis when snapshots exist.
// The offline path — `pgbot why` without --duration — is untouched.
func runWhyLive(ctx context.Context, w io.Writer, f whyFlags, dsnArg string) error {
	connString := firstNonEmpty(dsnArg, os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"))
	if connString == "" {
		return fmt.Errorf("--duration samples the live database: pass a connection string or set $DATABASE_URL")
	}
	duration, hz := clampWaits(f.duration, f.hz)

	cctx, cancel := context.WithTimeout(ctx, duration+30*time.Second)
	defer cancel()
	target, err := conn.Connect(cctx, connString)
	if err != nil {
		return err
	}
	defer target.Close()
	target.Warm(cctx)

	if !f.json {
		fmt.Fprintf(os.Stderr, "pgbot: sampling the live database for %s at %dHz...\n", duration, hz)
	}
	study := collect.RunWaitStudy(cctx, target, target.Caps, collect.WaitStudyOptions{Hz: hz, Window: duration})

	host, port := hostPort(target)
	fp := store.Fingerprint(host, port, target.Caps.Database, target.Caps.SystemIdentifier)

	// Baseline BEFORE folding this study in, so the window can't corroborate
	// itself: the previous 24h of rollups, ending 15 minutes ago.
	var hist *why.HistShares
	if st, err := store.Open(f.storePath); err == nil {
		now := time.Now().UTC()
		if shares, total, err := store.ReadWaitShares(st, fp, now.Add(-25*time.Hour), now.Add(-15*time.Minute)); err == nil && total > 0 {
			hist = &why.HistShares{Shares: shares, Samples: total, Desc: "the previous 24h of this database's history (local store)"}
		}
		if study.Profile != nil && study.Profile.Available {
			_ = st.SaveWaitProfile(fp, time.Now(), study.Profile)
		}
		_ = st.Close()
	}

	// Offline chains for the same database, best-effort: a store without
	// snapshots yet must not block the live diagnosis.
	report, err := computeWhy(f.storePath, fp, f.window, f.maxChains)
	if err != nil {
		report = why.Report{SchemaVersion: "1.1.0", Database: target.Caps.Database,
			Notes: []string{"no offline history for this database yet — each `pgbot inspect` adds one snapshot"}}
	}
	report.Live = why.ClassifyLive(study, hist)

	if f.json {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	printWhyLive(w, report, f)
	return nil
}

// printWhyLive renders the live diagnosis, then the historical chains in the
// offline renderer's exact voice.
func printWhyLive(w io.Writer, r why.Report, f whyFlags) {
	st := render.NewStyler(useColor(f.noColor))
	l := r.Live
	s := l.Study
	fmt.Fprintf(w, "%s\n", st.Head("Why is Postgres slow?"))
	if s != nil {
		fmt.Fprintf(w, "%s\n\n", st.Dim(fmt.Sprintf("sampled %.1fs at %dHz · coverage %d/%d polls · %d lock snapshots · source %s",
			s.WindowSeconds, s.Hz, s.Polls-s.PollFailures, s.Polls, s.LockSnapshots, l.Source)))
	}

	conf := ""
	if l.Confidence > 0 {
		conf = fmt.Sprintf("  ·  confidence %.0f%%", l.Confidence*100)
		if l.Confidence < 0.5 {
			conf += " (a possibility, not a diagnosis)"
		}
	}
	fmt.Fprintf(w, "%s %s%s\n\n", st.Head("Primary cause:"), l.Headline, st.Dim(conf))

	if len(l.Evidence) > 0 {
		fmt.Fprintln(w, st.Head("Live evidence"))
		for _, e := range l.Evidence {
			fmt.Fprintf(w, "  %s\n", e)
		}
		fmt.Fprintln(w)
	}
	if l.HistoryNote != "" {
		fmt.Fprintln(w, st.Head("Historical evidence"))
		fmt.Fprintf(w, "  %s\n\n", l.HistoryNote)
	}
	if l.NextCheck != "" {
		fmt.Fprintf(w, "%s %s\n\n", st.Head("Next check:"), l.NextCheck)
	}

	if len(r.Chains) > 0 || r.Snapshots >= 3 {
		fmt.Fprintln(w, st.Head("From stored history"))
		printWhy(w, r)
	} else {
		for _, note := range r.Notes {
			fmt.Fprintln(w, st.Dim(note))
		}
	}
}
