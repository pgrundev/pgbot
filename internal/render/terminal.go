// Package render turns a model.Context into the human-facing surfaces: the
// terminal report (grouped summary and --full section tables), the diff and
// advisor reports, and the --json contract writer.
package render

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/pgrundev/pgbot/internal/model"
)

// Options carries the presentation inputs the render layer shouldn't compute
// itself: whether color is wanted, sparkline series pulled from the baseline
// store, and where that store lives (shown in the footer).
type Options struct {
	Color        bool
	Trends       map[string][]float64
	BaselinePath string
	Width        int    // terminal width; clamped to a minimum of 80
	Full         bool   // --full: the section tables; default is the sentences-first summary
	Host         string // target host, for the header (optional)
}

// styler applies lipgloss styles, or no-ops when color is disabled (NO_COLOR,
// non-TTY, or --no-color).
type styler struct{ on bool }

func (s styler) c(code string, bold bool, text string) string {
	if !s.on {
		return text
	}
	st := lipgloss.NewStyle().Foreground(lipgloss.Color(code))
	if bold {
		st = st.Bold(true)
	}
	return st.Render(text)
}

// Styler exposes the render palette to sibling packages (e.g. the AI section in
// `pgbot explain`) so they match the rest of the report.
type Styler struct{ s styler }

// NewStyler returns a palette; color is off when false (NO_COLOR / non-TTY).
func NewStyler(color bool) Styler     { return Styler{styler{on: color}} }
func (x Styler) Dim(t string) string  { return x.s.dim(t) }
func (x Styler) Head(t string) string { return x.s.head(t) }
func (x Styler) Good(t string) string { return x.s.good(t) }
func (x Styler) Warn(t string) string { return x.s.warn(t) }
func (x Styler) Crit(t string) string { return x.s.crit(t) }
func (x Styler) AI(t string) string   { return x.s.c("212", true, t) } // magenta — distinctly "not the deterministic report"

// HumanBytes formats a byte count as KiB/MiB/GiB for sibling packages.
func HumanBytes(b int64) string { return humanBytes(b) }

func (s styler) dim(text string) string  { return s.c("240", false, text) }
func (s styler) head(text string) string { return s.c("39", true, text) }
func (s styler) crit(text string) string { return s.c("196", true, text) }
func (s styler) warn(text string) string { return s.c("208", true, text) }
func (s styler) info(text string) string { return s.c("245", false, text) }
func (s styler) good(text string) string { return s.c("42", false, text) }

// Terminal writes the human report. The default is sentences-first (T11): what
// needs attention, then the named checks that passed. --full adds the section
// tables. Nobody reads charts — the summary is the product.
func Terminal(w io.Writer, c *model.Context, opts Options) error {
	st := styler{on: opts.Color}
	width := opts.Width
	if width < 80 {
		width = 80 // spec: width-adaptive down to 80 columns
	}
	var b strings.Builder

	// Header: connected · <host|db> · postgres <ver> · read-only · <window> window.
	target := c.Server.Database
	if opts.Host != "" {
		target = opts.Host
	}
	fmt.Fprintf(&b, "%s · %s · %s · %s · %s window\n\n",
		st.good("connected"), st.head(target), pgLower(c.Server.VersionNum), st.dim("read-only"), windowLabel(c))

	// --profile=schema: state plainly that this is a schema check, so a clean
	// report is never mistaken for "this running database is healthy" (D3-1). A
	// schema profile skips every workload/history/infra finding — including
	// backups, replication, and wraparound — so silence here means nothing about
	// a live database.
	if c.Profile == "schema" {
		fmt.Fprintln(&b, st.warn("SCHEMA CHECK — catalog only."))
		fmt.Fprintln(&b, st.dim("Validates the schema of a (possibly empty) database. It says NOTHING about the"))
		fmt.Fprintln(&b, st.dim("health of a running database — not backups, replication, bloat, or wraparound."))
		fmt.Fprintln(&b)
	}

	if !c.Server.HasPgMonitor {
		fmt.Fprintln(&b, st.warn("! role lacks pg_monitor — some stats are partial. Fix: GRANT pg_monitor TO <role>;"))
		fmt.Fprintln(&b)
	}

	// Config warnings up top, not buried: a typo'd rule that silently doesn't
	// apply is the exact failure the config exists to prevent (B2-1).
	if len(c.ConfigWarnings) > 0 {
		fmt.Fprintln(&b, st.warn(fmt.Sprintf("! %d config warning(s) in .pgbot.toml:", len(c.ConfigWarnings))))
		for _, w := range c.ConfigWarnings {
			fmt.Fprintf(&b, "  %s\n", st.dim(w))
		}
		fmt.Fprintln(&b)
	}

	// Cold-window / reset surfacing — front and center, not buried. Irrelevant
	// under --profile=schema, which samples no counters, so skip it there.
	if c.Window.ColdWindow() && c.Profile != "schema" {
		age := int64(0)
		if c.Window.WindowAgeSeconds != nil {
			age = *c.Window.WindowAgeSeconds
		}
		reset := ""
		if c.Window.StatsResetAt != nil {
			reset = " (reset at " + c.Window.StatsResetAt.Format("15:04") + " — likely a compute restart)"
		}
		fmt.Fprintln(&b, st.warn(fmt.Sprintf("Statistics window: %s%s", shortDur(age), reset)))
		fmt.Fprintln(&b, st.dim("Counter-based findings (unused indexes, cache hit, seq scans) are suppressed until the window exceeds 15m."))
		fmt.Fprintln(&b)
	}
	if c.DeltaSuppressedReason != "" {
		fmt.Fprintln(&b, st.dim("Changes: "+c.DeltaSuppressedReason))
		fmt.Fprintln(&b)
	}

	if opts.Full {
		// Lead with the subsystem status board, then the detailed findings and
		// every section.
		renderBoard(&b, st, buildBoard(c))
		renderFindings(&b, st, c.Findings, width)
		renderHealth(&b, st, c, opts)
		renderActivity(&b, st, c)
		renderWaits(&b, st, c)
		renderLocks(&b, st, c)
		renderQueries(&b, st, c)
		renderTables(&b, st, c)
		renderIndexes(&b, st, c)
		renderInfra(&b, st, c)
		renderEvents(&b, st, c)
		renderChanges(&b, st, c)
		fmt.Fprintln(&b)
		if opts.BaselinePath != "" {
			fmt.Fprintln(&b, st.dim("baseline: "+opts.BaselinePath))
		}
	} else {
		// Graded, grouped summary: health score, CRITICAL/WARNING/NOTE, then GOOD.
		renderGrouped(&b, st, c, width)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// evidenceDisplayCap bounds how many aggregate rows the terminal prints; the
// full list is always in --json.
const evidenceDisplayCap = 10

func renderFindings(b *strings.Builder, st styler, fs []model.Finding, width int) {
	if len(fs) == 0 {
		fmt.Fprintln(b, st.good("✓ no findings — nothing stood out"))
		fmt.Fprintln(b)
		return
	}
	// Partition off findings hidden by config (suppressed non-criticals). A
	// suppressed critical stays in the main list, visibly marked (B2-2).
	var visible, suppressed []model.Finding
	for _, f := range fs {
		if f.Suppressed && f.Severity != model.SeverityCritical {
			suppressed = append(suppressed, f)
		} else {
			visible = append(visible, f)
		}
	}
	// Preexisting findings (--fail-on-new) render marked but never count in the
	// headline: the exit code and the default view both treat them as old news,
	// and the --full view of the same run must not contradict them.
	crit, warn, preexisting, live := 0, 0, 0, 0
	for _, f := range visible {
		if f.Preexisting {
			preexisting++
			continue
		}
		live++
		switch f.Severity {
		case model.SeverityCritical:
			crit++
		case model.SeverityWarn:
			warn++
		}
	}
	summary := fmt.Sprintf("%d finding(s)", live)
	if crit > 0 {
		summary += fmt.Sprintf(" · %d critical", crit)
	}
	if warn > 0 {
		summary += fmt.Sprintf(" · %d warning", warn)
	}
	if preexisting > 0 {
		summary += fmt.Sprintf(" · %d preexisting (not new)", preexisting)
	}
	if len(suppressed) > 0 {
		summary += fmt.Sprintf(" · %d suppressed", len(suppressed))
	}
	fmt.Fprintln(b, st.head(summary))
	for _, f := range visible {
		icon, color := "·", st.info
		switch f.Severity {
		case model.SeverityCritical:
			icon, color = "⛔", st.crit
		case model.SeverityWarn:
			icon, color = "⚠", st.warn
		}
		// Below 0.5 confidence we present a possibility, not an assertion.
		title := f.Title
		if f.Confidence > 0 && f.Confidence < 0.5 {
			title += st.dim(" (possible)")
		}
		fmt.Fprintf(b, "  %s %s\n", color(icon), color(title))
		if f.Suppressed { // a suppressed critical that still renders
			fmt.Fprintf(b, "     %s\n", st.dim("suppressed by config ("+f.SuppressionRule+"): "+suppReason(f)))
		}
		if f.Preexisting {
			fmt.Fprintf(b, "     %s\n", st.dim("preexisting — already in the base report, not introduced by this change"))
		}
		for _, line := range wrapText(f.Detail, width-5) {
			fmt.Fprintf(b, "     %s\n", st.dim(line))
		}
		if len(f.Evidence) > 0 {
			// Aggregate findings now store the full object list (so config can drop
			// individual rows); cap the DISPLAY here with a "+N more" tail.
			ev := f.Evidence
			tail := ""
			if len(ev) > evidenceDisplayCap {
				tail = fmt.Sprintf(", … +%d more", len(ev)-evidenceDisplayCap)
				ev = ev[:evidenceDisplayCap]
			}
			for _, line := range wrapText(strings.Join(ev, ", ")+tail, width-5) {
				fmt.Fprintf(b, "     %s\n", st.dim(line))
			}
		}
		// Caveats render inline under the finding, never in a footnote — these are
		// the "but…" clauses that stop a confident recommendation from causing harm.
		for _, cav := range f.Caveats {
			for i, line := range wrapText(cav, width-9) {
				prefix := "⚠ but "
				if i > 0 {
					prefix = "      "
				}
				fmt.Fprintf(b, "     %s\n", st.warn(prefix)+st.dim(line))
			}
		}
		if f.Remediation != "" {
			for i, line := range wrapText(f.Remediation, width-7) {
				prefix := "→ "
				if i > 0 {
					prefix = "  "
				}
				fmt.Fprintf(b, "     %s\n", st.dim(prefix+line))
			}
		}
		renderSafetyGuards(b, st, f, width, "     ")
		// Every finding references its catalogue page — offline via the subcommand.
		fmt.Fprintf(b, "     %s\n", st.dim("docs: pgbot explain-finding "+f.ID))
	}

	// Suppressed section: dimmed, collapsed, reason inline (B2-2). Kept visible in
	// --full so a muted finding is auditable, never silently gone.
	if len(suppressed) > 0 {
		fmt.Fprintln(b)
		fmt.Fprintln(b, st.dim(fmt.Sprintf("SUPPRESSED (%d) — hidden by .pgbot.toml", len(suppressed))))
		for _, f := range suppressed {
			obj := f.Object
			if obj == "" {
				obj = "(cluster)"
			}
			fmt.Fprintf(b, "  %s\n", st.dim(fmt.Sprintf("· [%s] %s — %s", f.Severity, f.Title, suppReason(f))))
			fmt.Fprintf(b, "    %s\n", st.dim(fmt.Sprintf("%s · rule: %s", obj, f.SuppressionRule)))
		}
	}
	fmt.Fprintln(b)
}

// windowLabel renders the stats-window age like "4h12m" / "3d4h" / "45m".
func windowLabel(c *model.Context) string {
	if c.Window.WindowAgeSeconds == nil {
		return "—"
	}
	s := *c.Window.WindowAgeSeconds
	switch {
	case s >= 86400:
		return fmt.Sprintf("%dd%dh", s/86400, (s%86400)/3600)
	case s >= 3600:
		return fmt.Sprintf("%dh%dm", s/3600, (s%3600)/60)
	case s >= 60:
		return fmt.Sprintf("%dm", s/60)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

func section(b *strings.Builder, st styler, name string, sec model.Section) bool {
	label := st.head(strings.ToUpper(name))
	tag := st.dim(sec.Exactness)
	fmt.Fprintf(b, "%s  %s\n", label, tag)
	if sec.Exactness == model.ExactnessUnavailable {
		fmt.Fprintf(b, "  %s\n\n", st.dim(sec.Reason))
		return false
	}
	return true
}

func renderHealth(b *strings.Builder, st styler, c *model.Context, opts Options) {
	if c.Health == nil {
		return
	}
	h := c.Health
	if !section(b, st, "health", h.Section) {
		return
	}
	fmt.Fprintf(b, "  TPS %s   cache hit %s   connections %d   rollbacks %s\n",
		f2(h.TPS, humanNum), f2(h.CacheHitRatio, pct), h.Connections, f2(h.RollbackRatio, pct))
	fmt.Fprintf(b, "  writes %s/s   returned %s/s   deadlocks %s/min\n",
		f2(h.TupWrittenPerS, humanNum), f2(h.TupReturnedPerS, humanNum), f2(h.DeadlocksPerMin, humanNum))
	if spark := sparkline(opts.Trends["tps"]); spark != "" {
		fmt.Fprintf(b, "  %s %s\n", spark, st.dim("tps, recent runs"))
	}
	if c.Window.StatsWindowDays != nil && *c.Window.StatsWindowDays > 30 {
		fmt.Fprintf(b, "  %s\n", st.dim(fmt.Sprintf("(ratios span %.0f days since last stats reset)", *c.Window.StatsWindowDays)))
	}
	fmt.Fprintln(b)
}

func renderActivity(b *strings.Builder, st styler, c *model.Context) {
	if c.Activity == nil {
		return
	}
	a := c.Activity
	if !section(b, st, "activity", a.Section) {
		return
	}
	fmt.Fprintf(b, "  %d total · %d active · %d idle · %d idle-in-txn · %d waiting\n",
		a.Total, a.Active, a.Idle, a.IdleInTransaction, a.Waiting)
	if a.LongestXactSec > 0 {
		fmt.Fprintf(b, "  longest transaction %.0fs · longest active query %.0fs\n", a.LongestXactSec, a.LongestActiveSec)
	}
	fmt.Fprintln(b)
}

// renderWaits shows the ASH profile: where active time went over the sampling
// window. It is honest about its sample size — a thin profile is labelled as
// indicative, never dressed up as a confident percentage breakdown.
func renderWaits(b *strings.Builder, st styler, c *model.Context) {
	w := c.WaitProfile
	if w == nil || !w.Available {
		return // disabled (--ash-hz 0) or sampler failed: say nothing here
	}
	if w.Samples == 0 {
		fmt.Fprintf(b, "%s  %s\n\n", st.head("WHERE TIME WENT"),
			st.dim(fmt.Sprintf("no active sessions in %ss of sampling — the database was idle", trimZero(w.WindowSeconds))))
		return
	}

	header := fmt.Sprintf("%d samples over %ss", w.Samples, trimZero(w.WindowSeconds))
	fmt.Fprintf(b, "%s  %s\n", st.head("WHERE TIME WENT"), st.dim(header))
	if w.Thin() {
		fmt.Fprintf(b, "  %s\n", st.warn(fmt.Sprintf("only %d samples — treat the shares below as indicative, not precise", w.Samples)))
	}

	// Flatten buckets to "Type:Event share%" lines (CPU has no event), share-desc.
	type line struct {
		label string
		share float64
		typ   string
	}
	var lines []line
	for _, bk := range w.Buckets {
		if bk.Type == "CPU" || len(bk.Events) == 0 {
			lines = append(lines, line{label: bk.Type, share: bk.Share, typ: bk.Type})
			continue
		}
		for _, ev := range bk.Events {
			lines = append(lines, line{label: bk.Type + ":" + ev.Event, share: ev.Share, typ: bk.Type})
		}
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].share > lines[j].share })
	if len(lines) > 6 {
		lines = lines[:6]
	}

	lockTop := topLockQuery(w)
	for _, ln := range lines {
		attr := ""
		if ln.typ == "Lock" && lockTop != nil {
			attr = st.dim(fmt.Sprintf("  query %s%s", queryTag(lockTop.QueryID), sampleParen(lockTop.SampleText)))
		}
		fmt.Fprintf(b, "  %-26s %3.0f%%%s\n", ln.label, ln.share*100, attr)
	}
	fmt.Fprintln(b)
}

// topLockQuery returns the query with the most Lock-wait samples, if any.
func topLockQuery(w *model.WaitProfile) *model.QueryWaits {
	var best *model.QueryWaits
	var bestN float64
	for i := range w.ByQuery {
		q := &w.ByQuery[i]
		n := q.LockShare * float64(q.Count)
		if n > bestN {
			best, bestN = q, n
		}
	}
	return best
}

func sampleParen(text string) string {
	if text == "" {
		return ""
	}
	return " (" + truncate(text, 32) + ")"
}

// queryTag mirrors findings.queryTag: the low 4 hex digits of a query_id.
func queryTag(id int64) string { return fmt.Sprintf("%04x", uint64(id)&0xffff) }

// trimZero formats a seconds value without a trailing ".0".
func trimZero(v float64) string {
	s := fmt.Sprintf("%.1f", v)
	if strings.HasSuffix(s, ".0") {
		return s[:len(s)-2]
	}
	return s
}

func renderLocks(b *strings.Builder, st styler, c *model.Context) {
	if c.Locks == nil || c.Locks.Exactness == model.ExactnessUnavailable {
		return
	}
	if c.Locks.BlockedCount == 0 {
		return // silence when clean; a blocking finding already fires when not
	}
	section(b, st, "locks", c.Locks.Section)
	tw := newTab(b)
	fmt.Fprintln(tw, "  blocked pid\tblocked by\twaited\tquery")
	for _, ch := range c.Locks.Chains {
		fmt.Fprintf(tw, "  %d\t%v\t%.0fs\t%s\n", ch.BlockedPID, ch.BlockingPIDs, ch.WaitSeconds, truncate(ch.BlockedQuery, 40))
	}
	tw.Flush()
	fmt.Fprintln(b)
}

func renderQueries(b *strings.Builder, st styler, c *model.Context) {
	if c.Queries == nil {
		return
	}
	if !section(b, st, "queries", c.Queries.Section) {
		return
	}
	if len(c.Queries.Top) == 0 {
		fmt.Fprintf(b, "  %s\n\n", st.dim("no query activity recorded yet"))
		return
	}
	tw := newTab(b)
	fmt.Fprintln(tw, "  calls\ttotal\tmean\tquery")
	shown := len(c.Queries.Top)
	if shown > 8 {
		shown = 8
	}
	for _, q := range c.Queries.Top[:shown] {
		fmt.Fprintf(tw, "  %s\t%s ms\t%.2f ms\t%s\n", humanNum(float64(q.Calls)), humanNum(q.TotalMS), q.MeanMS, truncate(q.Query, 48))
	}
	tw.Flush()
	if len(c.Queries.Top) > shown {
		fmt.Fprintf(b, "  %s\n", st.dim(fmt.Sprintf("… and %d more (see --json)", len(c.Queries.Top)-shown)))
	}
	fmt.Fprintln(b)
}

func renderTables(b *strings.Builder, st styler, c *model.Context) {
	if c.Tables == nil {
		return
	}
	if !section(b, st, "tables", c.Tables.Section) {
		return
	}
	fmt.Fprintf(b, "  database size %s\n", humanBytes(c.Tables.DBSizeBytes))
	tw := newTab(b)
	fmt.Fprintln(tw, "  table\tsize\trows\tdead\tseq/idx scans")
	for i, t := range c.Tables.Top {
		if i >= 6 {
			break
		}
		fmt.Fprintf(tw, "  %s.%s\t%s\t%s\t%s\t%s/%s\n",
			t.Schema, t.Name, humanBytes(t.TotalBytes), humanNum(float64(t.LiveTuples)),
			pct(t.DeadRatio), humanNum(float64(t.SeqScans)), humanNum(float64(t.IndexScans)))
	}
	tw.Flush()
	fmt.Fprintln(b)
}

func renderIndexes(b *strings.Builder, st styler, c *model.Context) {
	if c.Indexes == nil {
		return
	}
	if !section(b, st, "indexes", c.Indexes.Section) {
		return
	}
	var unusedBytes int64
	for _, ix := range c.Indexes.Unused {
		unusedBytes += ix.Bytes
	}
	fmt.Fprintf(b, "  %d total · %s unused (%s)\n", c.Indexes.Total, humanNum(float64(len(c.Indexes.Unused))), humanBytes(unusedBytes))
	fmt.Fprintln(b)
}

func renderInfra(b *strings.Builder, st styler, c *model.Context) {
	// WAL / IO / replication / settings condensed to a couple of lines each.
	if c.WAL != nil && c.WAL.Exactness == model.ExactnessSampled {
		fmt.Fprintf(b, "%s  %s   WAL %s/s\n", st.head("WAL"), st.dim(c.WAL.Exactness), f2(c.WAL.BytesPerSec, humanBytes2))
	}
	if c.IO != nil && c.IO.Exactness == model.ExactnessSampled {
		fmt.Fprintf(b, "%s   %s   buffers written %s/s · checkpoints %d timed / %d req\n",
			st.head("IO"), st.dim(c.IO.Exactness), f2(c.IO.BuffersWrittenPerS, humanNum), c.IO.CheckpointsTimed, c.IO.CheckpointsReq)
	}
	if c.Replication != nil && c.Replication.Exactness == model.ExactnessScraped {
		if c.Replication.IsReplica {
			fmt.Fprintf(b, "%s  %s   replica · receiver lag %s\n", st.head("REPLICATION"), st.dim("scraped"), f2(c.Replication.ReceiverLagSec, func(v float64) string { return fmt.Sprintf("%.1fs", v) }))
		} else if len(c.Replication.Replicas) > 0 {
			fmt.Fprintf(b, "%s  %s   %d standby(s) connected\n", st.head("REPLICATION"), st.dim("scraped"), len(c.Replication.Replicas))
		}
	}
	if c.Settings != nil && len(c.Settings.Overrides) > 0 {
		keys := make([]string, 0, len(c.Settings.Overrides))
		for k := range c.Settings.Overrides {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(b, "%s  %s   %d non-default parameters (see --json)\n", st.head("SETTINGS"), st.dim("scraped"), len(keys))
	}
	fmt.Fprintln(b)
}

func renderEvents(b *strings.Builder, st styler, c *model.Context) {
	if len(c.Events) == 0 {
		return
	}
	fmt.Fprintf(b, "%s\n", st.head("WHAT CHANGED"))
	for _, e := range c.Events {
		fmt.Fprintf(b, "  %s %s\n", st.dim("·"), eventLine(e))
	}
	fmt.Fprintln(b)
}

// eventLine renders an event with honest timing: a real timestamp when we have
// one (reset/restart, confidence 1.0), otherwise a "between A and B" range —
// never a precise time we merely inferred.
func eventLine(e model.Event) string {
	subject := e.Object
	verb := strings.TrimPrefix(e.Kind, "schema.")
	verb = strings.ReplaceAll(verb, "_", " ")

	var change string
	switch {
	case e.Kind == "config.changed" && e.Before != "" && e.After != "":
		change = fmt.Sprintf("config %s %s → %s", subject, e.Before, e.After)
	case e.Kind == "config.changed":
		change = fmt.Sprintf("config %s %s%s", subject, e.Before, e.After) // one side empty
	case e.Kind == "server.restarted":
		change = "server restarted"
	case e.Kind == "stats.reset":
		change = "statistics reset"
	default:
		change = fmt.Sprintf("%s %s", subject, verb) // e.g. "public.orders.idx dropped"
	}

	when := ""
	switch {
	case e.Confidence >= 1.0 && e.OccurredAfter != nil:
		when = " at " + e.OccurredAfter.Format("15:04")
	case e.OccurredAfter != nil && e.OccurredBefore != nil:
		when = fmt.Sprintf(" between %s and %s", e.OccurredAfter.Format("15:04"), e.OccurredBefore.Format("15:04"))
	}
	return change + when
}

func renderChanges(b *strings.Builder, st styler, c *model.Context) {
	if c.Deltas == nil {
		return
	}
	if len(c.Deltas.Changes) == 0 {
		fmt.Fprintf(b, "%s  %s\n", st.head("CHANGES"), st.dim("nothing material changed since "+c.Deltas.Against.Format("15:04")))
		return
	}
	fmt.Fprintf(b, "%s since %s\n", st.head("CHANGES"), c.Deltas.Against.Format("2006-01-02 15:04"))
	for _, d := range c.Deltas.Changes {
		color := st.info
		if d.Severity == model.SeverityWarn {
			color = st.warn
		}
		change := fmt.Sprintf("%s → %s", humanNum(d.Before), humanNum(d.After))
		if d.PctChange != nil {
			change += fmt.Sprintf(" (%+.0f%%)", *d.PctChange*100)
		}
		fmt.Fprintf(b, "  %s %s  %s  %s\n", color("·"), d.Subject, change, st.dim(d.Note))
	}
}

// helpers

func newTab(b *strings.Builder) *tabwriter.Writer {
	return tabwriter.NewWriter(b, 0, 2, 2, ' ', 0)
}

func truncate(s string, n int) string {
	// Collapse all internal whitespace and truncate by RUNE so a multibyte
	// character is never split (which misaligns columns and prints a replacement
	// glyph). PR#1.
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n || n < 1 {
		return s
	}
	return string(r[:n-1]) + "…"
}

func humanBytes2(v float64) string { return humanBytes(int64(v)) }

var _ = time.Now // keep time imported for future use
