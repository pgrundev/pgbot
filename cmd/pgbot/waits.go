package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/pgrundev/pgbot/internal/collect"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/render"
	"github.com/pgrundev/pgbot/internal/store"
	"github.com/spf13/cobra"
)

type waitsFlags struct {
	duration   time.Duration
	hz         int
	group      string
	pid        int
	jsonOut    bool
	noStore    bool
	rawQueries bool
	noColor    bool
}

// newWaitsCmd — `pgbot waits`. Sample where database time goes for a bounded
// window: wait classes, top events, waiting sessions and queries, and blockers
// — named only with sustained evidence. Everything is a share of a SAMPLED
// window; the only exact numbers are ages read from the server.
func newWaitsCmd() *cobra.Command {
	var f waitsFlags
	cmd := &cobra.Command{
		Use:   "waits <connection-string>",
		Short: "Sample where database time goes — waits, blockers, contention (experimental)",
		Long: `Samples pg_stat_activity at --hz and the lock graph at 1 Hz for --duration,
then reports average active sessions, DB time by wait class, top wait events,
waiting sessions, and blockers. A blocker is named only when the same holder is
seen across several lock snapshots — one glimpse is reported as transient, not
blamed. Results are SAMPLED shares of the window, never measured durations.
Aggregated wait counts are saved to the local baseline store (--no-store skips).`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWaits(cmd, args, f)
		},
	}
	fl := cmd.Flags()
	fl.DurationVar(&f.duration, "duration", 10*time.Second, "sampling window (clamped to 1s–5m)")
	fl.IntVar(&f.hz, "hz", 10, "activity sampling rate (clamped to 1–20)")
	fl.StringVar(&f.group, "group", "event", "which breakdown leads: event, query, or session")
	fl.IntVar(&f.pid, "pid", 0, "focus on one backend PID (its blockers included)")
	fl.BoolVar(&f.jsonOut, "json", false, "emit the versioned waits document (scrubbed)")
	fl.BoolVar(&f.noStore, "no-store", false, "don't fold wait counts into the local baseline store")
	fl.BoolVar(&f.rawQueries, "raw-query-text", false, "show query text verbatim instead of scrubbed (terminal use)")
	fl.BoolVar(&f.noColor, "no-color", false, "disable ANSI color")
	return cmd
}

// clampWaits bounds the study so sampling overhead stays predictable.
func clampWaits(d time.Duration, hz int) (time.Duration, int) {
	if d < time.Second {
		d = time.Second
	}
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	if hz < 1 {
		hz = 1
	}
	if hz > 20 {
		hz = 20
	}
	return d, hz
}

type waitsGroup string

func parseWaitsGroup(s string) (waitsGroup, error) {
	switch s {
	case "event", "query", "session":
		return waitsGroup(s), nil
	}
	return "", usageErrf("unknown --group %q (valid: event, query, session)", s)
}

func runWaits(cmd *cobra.Command, args []string, f waitsFlags) error {
	connString := firstNonEmpty(argAt(args, 0), os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"))
	if connString == "" {
		return fmt.Errorf("no connection string (pass one or set $DATABASE_URL)")
	}
	group, err := parseWaitsGroup(f.group)
	if err != nil {
		return err
	}
	duration, hz := clampWaits(f.duration, f.hz)
	if f.jsonOut {
		// The JSON document is a machine contract and is always scrubbed;
		// --raw-query-text is a terminal-only convenience.
		f.rawQueries = false
	}

	// Window + connect slack; Ctrl+C still ends early with partial coverage.
	ctx, cancel := context.WithTimeout(cmd.Context(), duration+30*time.Second)
	defer cancel()

	target, err := conn.Connect(ctx, connString)
	if err != nil {
		return err
	}
	defer target.Close()
	target.Warm(ctx) // register every own PID before sampling starts

	if !f.jsonOut {
		fmt.Fprintf(os.Stderr, "pgbot: sampling for %s at %dHz (activity) + 1Hz (locks)...\n", duration, hz)
	}
	study := collect.RunWaitStudy(ctx, target, target.Caps, collect.WaitStudyOptions{
		Hz: hz, Window: duration, FocusPID: f.pid, RawQueryText: f.rawQueries,
	})

	if !f.noStore && study.Profile != nil && study.Profile.Available {
		host, port := hostPort(target)
		fp := store.Fingerprint(host, port, target.Caps.Database, target.Caps.SystemIdentifier)
		if st, err := store.Open(""); err == nil {
			_ = st.SaveWaitProfile(fp, time.Now(), study.Profile)
			_ = st.Close()
		}
	}

	if f.jsonOut {
		b, err := json.MarshalIndent(study, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	renderWaits(study, group, target, f)
	return nil
}

func renderWaits(s *model.WaitStudy, group waitsGroup, target *conn.Target, f waitsFlags) {
	st := render.NewStyler(useColor(f.noColor))
	host, _ := hostPort(target)

	fmt.Printf("%s · %s · %.1fs window · %dHz\n", st.Head("pgbot waits"), st.Dim(host), s.WindowSeconds, s.Hz)
	fmt.Println(st.Dim(fmt.Sprintf("sampled %d/%d polls · %d/%d lock snapshots · profile: sampled (shares of the window, not exact timing)",
		s.Polls-s.PollFailures, s.Polls, s.LockSnapshots, s.LockSnapshots+s.LockSnapshotFails)))
	if s.Partial != "" {
		fmt.Println(st.Warn("partial: " + s.Partial))
	}
	fmt.Println()

	if s.Profile == nil || !s.Profile.Available {
		reason := "wait sampling unavailable"
		if s.Profile != nil && s.Profile.Reason != "" {
			reason = s.Profile.Reason
		}
		fmt.Println(st.Warn(reason))
		return
	}

	fmt.Printf("Average active sessions: %s\n\n", st.Head(fmt.Sprintf("%.1f", s.AAS)))

	if len(s.Profile.Buckets) > 0 {
		fmt.Println(st.Head("DB time by wait class"))
		for _, b := range s.Profile.Buckets {
			bar := strings.Repeat("█", int(b.Share*20+0.5))
			fmt.Printf("  %-10s %4.0f%%  %s\n", b.Type, b.Share*100, st.Dim(bar))
		}
		fmt.Println()
	}

	switch group {
	case "event":
		renderTopEvents(st, s)
	case "query":
		renderTopQueries(st, s)
	case "session":
		renderTopSessions(st, s)
	}

	for _, b := range s.Blockers {
		fmt.Println(st.Head("Blocked → blocker (sustained evidence)"))
		renderBlocker(st, b, s)
	}
	if len(s.Transient) > 0 {
		fmt.Println(st.Dim(fmt.Sprintf("transient lock waits: %d holder(s) seen too briefly to name as a cause", len(s.Transient))))
		fmt.Println()
	}

	fmt.Println(st.Head("Conclusion"))
	fmt.Println("  " + strings.ReplaceAll(waitsConclusion(s), "\n", "\n  "))
}

func renderTopEvents(st render.Styler, s *model.WaitStudy) {
	var evs []model.WaitEvent
	var types []string
	for _, b := range s.Profile.Buckets {
		for _, e := range b.Events {
			evs = append(evs, e)
			types = append(types, b.Type)
		}
	}
	if len(evs) == 0 {
		return
	}
	idx := make([]int, len(evs))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return evs[idx[a]].Count > evs[idx[b]].Count })
	fmt.Println(st.Head("Top wait events"))
	for n, i := range idx {
		if n == 8 {
			break
		}
		fmt.Printf("  %-32s %4.0f%%  %s\n", types[i]+":"+evs[i].Event, evs[i].Share*100,
			st.Dim(fmt.Sprintf("%d samples", evs[i].Count)))
	}
	fmt.Println()
}

func renderTopQueries(st render.Styler, s *model.WaitStudy) {
	if len(s.Profile.ByQuery) == 0 {
		fmt.Println(st.Dim("no per-query attribution (query_id needs PostgreSQL 14+)"))
		fmt.Println()
		return
	}
	fmt.Println(st.Head("Top waiting queries"))
	for n, q := range s.Profile.ByQuery {
		if n == 8 {
			break
		}
		fmt.Printf("  %4.0f%%  %-24s %s\n", q.Share*100, st.Dim(q.TopEvent), truncStr(q.SampleText, 70))
	}
	fmt.Println()
}

func renderTopSessions(st render.Styler, s *model.WaitStudy) {
	if len(s.Sessions) == 0 {
		return
	}
	fmt.Println(st.Head("Top sessions"))
	for n, sess := range s.Sessions {
		if n == 8 {
			break
		}
		id := fmt.Sprintf("PID %d", sess.PID)
		if sess.User != "" {
			id += " " + sess.User + "@" + sess.DB
		}
		fmt.Printf("  %4.0f%%  %-28s %-20s %s\n", sess.Share*100, id, st.Dim(sess.TopEvent), truncStr(sess.SampleText, 50))
	}
	fmt.Println()
}

func renderBlocker(st render.Styler, b model.Blocker, s *model.WaitStudy) {
	for _, v := range b.Victims {
		fmt.Printf("  PID %d  %s\n", v.PID, truncStr(v.Query, 70))
		if share := victimLockShareOf(s, v.PID); share > 0 {
			fmt.Printf("    ~%.0f%% of its sampled time in Lock:%s\n", share*100, v.WaitEvent)
		}
	}
	holder := fmt.Sprintf("blocked by PID %d (%s, xact age %.0fs", b.HolderPID, b.HolderState, b.HolderXactAgeS)
	if b.HolderApp != "" {
		holder += ", app=" + b.HolderApp
	}
	fmt.Println("    " + st.Warn(holder+")") + st.Dim(fmt.Sprintf("  · seen in %d snapshots", b.Observations)))
	if b.HolderQuery != "" {
		fmt.Printf("      holder's last query: %s\n", st.Dim(truncStr(b.HolderQuery, 70)))
	}
	fmt.Println()
}

func victimLockShareOf(s *model.WaitStudy, pid int) float64 {
	for _, sess := range s.Sessions {
		if sess.PID == pid && sess.Count > 0 && strings.HasPrefix(sess.TopEvent, "Lock:") {
			return sess.Share
		}
	}
	return 0
}

// waitsConclusion is the evidence-gated bottom line. It never claims exact
// timing, refuses to conclude from a thin sample, and never recommends an
// index for lock contention — that is the entire point of the command.
func waitsConclusion(s *model.WaitStudy) string {
	if s.Profile == nil || !s.Profile.Available {
		return "sampling failed — no conclusion. Check connectivity and permissions, then re-run."
	}
	if s.Profile.Samples == 0 {
		return "the database was idle during the window: no active sessions were sampled."
	}
	if s.Thin {
		return fmt.Sprintf("too few samples (%d) to conclude anything — re-run with a longer --duration.", s.Profile.Samples)
	}
	top := s.Profile.Buckets[0]
	if len(s.Blockers) > 0 && top.Type == "Lock" {
		b := s.Blockers[0]
		return fmt.Sprintf("transaction contention — sampled time went mostly to waiting on locks (%.0f%%),\n"+
			"and PID %d (%s, xact age %.0fs) held them across %d observations.\n"+
			"This is not evidence of a missing index. Ages are exact; shares are sampled.",
			top.Share*100, b.HolderPID, b.HolderState, b.HolderXactAgeS, b.Observations)
	}
	switch top.Type {
	case "CPU":
		return fmt.Sprintf("mostly on-CPU (%.0f%% of samples) — the server is executing, not waiting.", top.Share*100)
	case "IO":
		return fmt.Sprintf("mostly IO (%.0f%% of samples) — time went to reading/writing data, not lock contention.\n"+
			"If one query dominates, `pgbot advise` can check indexes with planner validation.", top.Share*100)
	default:
		return fmt.Sprintf("dominant wait class: %s (%.0f%% of samples).", top.Type, top.Share*100)
	}
}
