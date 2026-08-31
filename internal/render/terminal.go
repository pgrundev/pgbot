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

	// Header: connected · <host|db> · <engine/version> · read-only · <window> window.
	target := c.Server.Database
	if opts.Host != "" {
		target = opts.Host
	}
	fmt.Fprintf(&b, "%s · %s · %s · %s · %s window\n\n",
		st.good("connected"), st.head(target), serverLabel(c.Server), st.dim("read-only"), windowLabel(c))

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
	if c.Server.Engine == "cockroachdb" {
		if cockroachClusterHealthAvailable(c) {
			fmt.Fprintln(&b, st.warn("COCKROACHDB PREVIEW — cluster health and SQL workload diagnostics."))
		} else {
			fmt.Fprintln(&b, st.warn("COCKROACHDB PREVIEW — SQL workload diagnostics; configure --crdb-admin-url for cluster health."))
		}
		fmt.Fprintln(&b)
	}

	if c.Server.Engine == "cockroachdb" && !c.Server.HasViewActivity {
		fmt.Fprintln(&b, st.warn("! role lacks VIEWACTIVITY — only its own sessions are visible. Fix: GRANT SYSTEM VIEWACTIVITY TO <role>;"))
		fmt.Fprintln(&b)
	} else if c.Server.Engine != "cockroachdb" && !c.Server.HasPgMonitor {
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
		if c.Server.Engine == "cockroachdb" && len(c.Findings) == 0 {
			fmt.Fprintln(&b, st.good("✓ no findings from supported CockroachDB checks"))
			fmt.Fprintln(&b)
		} else {
			renderFindings(&b, st, c.Findings, width)
		}
		renderHealth(&b, st, c, opts)
		renderCockroachDistribution(&b, st, c)
		renderCockroachStorage(&b, st, c)
		renderCockroachJobs(&b, st, c)
		renderActivity(&b, st, c)
		renderCockroachLiveQueries(&b, st, c)
		renderCockroachContention(&b, st, c)
		renderWaits(&b, st, c)
		renderLocks(&b, st, c)
		renderQueries(&b, st, c)
		renderCockroachInsights(&b, st, c)
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

// CockroachScreen writes one first-class CockroachDB diagnostic screen using
// the same section renderers as `inspect --full`. Dedicated commands call this
// entry point so their output cannot drift from the comprehensive report.
func CockroachScreen(w io.Writer, c *model.Context, name string, opts Options) error {
	if c == nil || c.Server.Engine != "cockroachdb" {
		return fmt.Errorf("%s screen requires a CockroachDB connection", name)
	}
	st := styler{on: opts.Color}
	var b strings.Builder
	target := c.Server.Database
	if opts.Host != "" {
		target = opts.Host
	}
	fmt.Fprintf(&b, "%s · %s · %s · %s\n\n",
		st.good("connected"), st.head(target), serverLabel(c.Server), st.dim("read-only"))

	switch name {
	case "health":
		renderHealth(&b, st, c, Options{Full: true})
	case "distribution":
		renderCockroachDistribution(&b, st, c)
	case "storage":
		renderCockroachStorage(&b, st, c)
	case "jobs":
		renderCockroachJobs(&b, st, c)
	case "activity":
		renderActivity(&b, st, c)
		renderCockroachLiveQueries(&b, st, c)
	case "contention":
		renderCockroachContention(&b, st, c)
	case "queries":
		renderQueries(&b, st, c)
	case "tables":
		renderTables(&b, st, c)
	case "indexes":
		var indexFindings []model.Finding
		for _, finding := range c.Findings {
			if strings.Contains(finding.ID, "index") {
				indexFindings = append(indexFindings, finding)
			}
		}
		if len(indexFindings) > 0 {
			width := opts.Width
			if width < 80 {
				width = 80
			}
			renderFindings(&b, st, indexFindings, width)
		}
		renderIndexes(&b, st, c)
	default:
		return fmt.Errorf("unknown CockroachDB screen %q", name)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func renderCockroachContention(b *strings.Builder, st styler, c *model.Context) {
	if c.Cockroach == nil {
		return
	}
	h := &c.Cockroach.Contention
	name := "contention"
	if h.WindowMinutes > 0 {
		name = fmt.Sprintf("contention — last %dm", h.WindowMinutes)
	}
	if !section(b, st, name, h.Section) {
		return
	}
	fmt.Fprintf(b, "  %d events · %s total wait · %s longest · %d serialization conflicts\n",
		h.TotalEvents, humanDurationMS(h.TotalWaitMS), humanDurationMS(h.MaxWaitMS), h.SerializationConflicts)
	fmt.Fprintln(b, st.dim("  bounded in-memory event store; raw keys and transaction IDs intentionally omitted"))
	if len(h.Hotspots) > 0 {
		t := newTab(b)
		fmt.Fprintln(t, "  object\ttype\tevents\ttotal\tmax\twaiter\tblocker txn\tlast seen")
		for _, x := range h.Hotspots {
			fmt.Fprintf(t, "  %s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
				truncate(contentionObject(x), 52), x.Type, x.Events, humanDurationMS(x.TotalWaitMS), humanDurationMS(x.MaxWaitMS),
				shortFingerprint(x.WaitingStatementFingerprint), contentionBlockerFingerprint(x), x.LastSeen.Format("15:04:05"))
		}
		t.Flush()
		fmt.Fprintln(b, st.dim("  query attribution: retained SQL statistics; blocker statement lists are unordered and capped at 5"))
		for _, x := range h.Hotspots[:min(5, len(h.Hotspots))] {
			fmt.Fprintf(b, "  %s\n", truncate(contentionObject(x), 72))
			fmt.Fprintf(b, "    %s\n", contentionWaiterSummary(x, 108))
			fmt.Fprintf(b, "    %s\n", contentionBlockerSummary(x, 108))
		}
	}
	fmt.Fprintln(b)
}

func contentionObject(h model.CockroachContentionHotspot) string {
	name := strings.Join([]string{h.Database, h.Schema, h.Table}, ".")
	if h.Index != "" {
		name += "/" + h.Index
	}
	return name
}

func shortFingerprint(s string) string {
	if s == "" {
		return "unresolved"
	}
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func contentionBlockerFingerprint(h model.CockroachContentionHotspot) string {
	if h.BlockingTxnFingerprint == "" && h.BlockerResolution == model.CockroachContentionNotResolved {
		return "not resolved by CockroachDB"
	}
	return shortFingerprint(h.BlockingTxnFingerprint)
}

func contentionWaiterSummary(h model.CockroachContentionHotspot, width int) string {
	if h.WaitingQuery != "" {
		return fmt.Sprintf("waiter · %s · %s", contentionApps(h.WaitingApplications), truncate(h.WaitingQuery, width))
	}
	return "waiter · " + contentionResolutionText(h.WaiterResolution)
}

func contentionBlockerSummary(h model.CockroachContentionHotspot, width int) string {
	query := h.BlockingQuery
	count := 0
	if query == "" && len(h.BlockingQueries) > 0 {
		query = h.BlockingQueries[0]
		count = len(h.BlockingQueries) - 1
	}
	if query != "" {
		suffix := ""
		if count > 0 {
			suffix = fmt.Sprintf(" (+%d statements)", count)
		}
		return fmt.Sprintf("blocker · %s · %s%s", contentionApps(h.BlockingApplications), truncate(query, width), suffix)
	}
	return "blocker · " + contentionResolutionText(h.BlockerResolution)
}

func contentionApps(apps []string) string {
	if len(apps) == 0 {
		return "app unknown"
	}
	return "app " + strings.Join(apps, ", ")
}

func contentionResolutionText(status string) string {
	switch status {
	case model.CockroachContentionNotResolved:
		return "not resolved by CockroachDB"
	case model.CockroachContentionNotFound:
		return "query not found in retained SQL statistics"
	case model.CockroachContentionStatsUnavailable:
		return "query attribution unavailable"
	case model.CockroachContentionResolved:
		return "resolved (query text unavailable)"
	default:
		return "unresolved"
	}
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
		if c.Server.Engine == "cockroachdb" && c.Window.SampleSeconds > 0 {
			return trimZero(c.Window.SampleSeconds) + "s sample"
		}
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
	if c.Server.Engine == "cockroachdb" && h.Cockroach != nil {
		renderCockroachHealth(b, st, h.Cockroach, opts.Full)
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

func cockroachClusterHealthAvailable(c *model.Context) bool {
	return c.Health != nil && c.Health.Cockroach != nil &&
		(c.Health.Cockroach.AdminAPI.Exactness != model.ExactnessUnavailable ||
			c.Health.Cockroach.Prometheus.Exactness != model.ExactnessUnavailable)
}

func renderCockroachHealth(b *strings.Builder, st styler, h *model.CockroachHealth, full bool) {
	if h.NodesTotal > 0 {
		fmt.Fprintf(b, "  nodes %d/%d live · %d stores · ranges %d unavailable / %d under-replicated\n",
			h.NodesLive, h.NodesTotal, h.StoresTotal, h.UnavailableRanges, h.UnderreplicatedRanges)
	}
	if h.CapacityBytes > 0 {
		fmt.Fprintf(b, "  capacity %s free of %s · fullest store %s\n",
			humanBytes(h.AvailableBytes), humanBytes(h.CapacityBytes), pct(h.MaxStoreUsedRatio))
	}
	load := "load —"
	if h.QueriesPerSec != nil {
		load = humanNum(*h.QueriesPerSec) + " queries/s"
	}
	fmt.Fprintf(b, "  %s · %d SQL connections", load, h.SQLConnections)
	if h.NewConnectionsPerSec != nil {
		fmt.Fprintf(b, " · %s new connections/s", humanNum(*h.NewConnectionsPerSec))
	}
	fmt.Fprintln(b)
	if h.MaxCPUPercent > 0 || h.MaxMemoryUsedRatio > 0 || h.ServiceLatencyP99MS > 0 || h.AdmissionWaitP99MS > 0 {
		fmt.Fprintf(b, "  peak CPU %.1f%% · memory %s · SQL p99 %.1fms · admission p99 %.1fms",
			h.MaxCPUPercent, pct(h.MaxMemoryUsedRatio), h.ServiceLatencyP99MS, h.AdmissionWaitP99MS)
		if h.AdmissionQueueMax > 0 {
			fmt.Fprintf(b, " · max queue %d", h.AdmissionQueueMax)
		}
		fmt.Fprintln(b)
	}
	fmt.Fprintf(b, "  sources admin %s · prometheus %s · jobs %s · hot ranges %s\n",
		sourceState(h.AdminAPI), sourceState(h.Prometheus), sourceState(h.Jobs), sourceState(h.HotRanges))

	if full {
		renderCockroachHealthTables(b, h)
	}
	fmt.Fprintln(b)
}

func sourceState(s model.Section) string {
	if s.Exactness == "" || s.Exactness == model.ExactnessUnavailable {
		return "unavailable"
	}
	if s.Reason != "" {
		return s.Exactness + " (partial)"
	}
	return s.Exactness
}

func renderCockroachHealthTables(b *strings.Builder, h *model.CockroachHealth) {
	if len(h.Nodes) > 0 {
		fmt.Fprintln(b, "\n  NODES")
		t := newTab(b)
		fmt.Fprintln(t, "  node\tstatus\tlocality\tversion\tCPU\tmemory\tSQL conns")
		for _, n := range h.Nodes {
			fmt.Fprintf(t, "  n%d\t%s\t%s\t%s\t%.1f%%\t%s\t%d\n", n.NodeID, n.Status,
				orDash(n.Locality), orDash(n.Version), n.CPUPercent, pct(n.MemoryUsedRatio), n.SQLConnections)
		}
		t.Flush()
	}
}

func renderCockroachDistribution(b *strings.Builder, st styler, c *model.Context) {
	if c.Server.Engine != "cockroachdb" || c.Health == nil || c.Health.Cockroach == nil {
		return
	}
	h := c.Health.Cockroach
	d := &h.Distribution
	if d.Exactness == "" {
		return
	}
	if !section(b, st, "distribution & balance", d.Section) {
		return
	}
	fmt.Fprintf(b, "  %d live stores · %d comparable by capacity · %d excluded on non-live nodes\n",
		d.LiveStores, d.ComparableStores, d.ExcludedStores)
	if d.ComparableStores > 0 {
		fmt.Fprintf(b, "  replicas %d–%d (mean %.1f) · leases %d–%d (mean %.1f)\n",
			d.ReplicaMin, d.ReplicaMax, d.ReplicaMean, d.LeaseMin, d.LeaseMax, d.LeaseMean)
	}
	if d.LiveStores > 0 {
		fmt.Fprintf(b, "  utilization %s–%s · %.1f percentage-point spread\n",
			pct(d.CapacityUsedMinRatio), pct(d.CapacityUsedMaxRatio), d.CapacityUsedSpread*100)
	}
	if d.HotRangeLeaseholderSamples > 0 {
		fmt.Fprintf(b, "  top hot ranges: n%d leaseholds %d/%d and %.1f%% of sampled CPU\n",
			d.HottestLeaseholderNodeID, d.HottestLeaseholderRanges, d.HotRangeLeaseholderSamples, d.HottestLeaseholderCPUShare*100)
	}
	if d.Reason != "" {
		fmt.Fprintln(b, st.dim("  "+d.Reason))
	}
	stores := append([]model.CockroachStoreBalance(nil), d.Stores...)
	sort.Slice(stores, func(i, j int) bool {
		if stores[i].Comparable != stores[j].Comparable {
			return stores[i].Comparable
		}
		if stores[i].UsedRatio != stores[j].UsedRatio {
			return stores[i].UsedRatio > stores[j].UsedRatio
		}
		return stores[i].StoreID < stores[j].StoreID
	})
	if len(stores) > 0 {
		t := newTab(b)
		fmt.Fprintln(t, "  store\tnode\tstatus\tcompared\tused\treplicas\tleases\tnode CPU\ttop hot\tlocality")
		for _, s := range stores[:min(30, len(stores))] {
			compared := "no"
			if s.Comparable {
				compared = "yes"
			}
			hot := "—"
			if s.TopHotRanges > 0 {
				hot = fmt.Sprintf("%d / %.3f CPU", s.TopHotRanges, s.TopHotCPUCores)
			}
			fmt.Fprintf(t, "  s%d\tn%d\t%s\t%s\t%s\t%d\t%d\t%.1f%%\t%s\t%s\n",
				s.StoreID, s.NodeID, orDash(s.Status), compared, pct(s.UsedRatio), s.RangeReplicas,
				s.Leaseholders, s.NodeCPUPercent, hot, orDash(s.Locality))
		}
		t.Flush()
	}
	if len(h.Hot) > 0 {
		fmt.Fprintln(b, "\n  HOTTEST PHYSICAL RANGES")
		t := newTab(b)
		fmt.Fprintln(t, "  range\tleaseholder\tCPU cores\tQPS\treads/s\twrites/s\ttable/index")
		for _, r := range h.Hot {
			name := strings.Join(r.Tables, ",")
			if len(r.Indexes) > 0 {
				name += "/" + strings.Join(r.Indexes, ",")
			}
			fmt.Fprintf(t, "  r%d\tn%d\t%.3f\t%s\t%s\t%s\t%s\n", r.RangeID, r.LeaseholderNodeID,
				r.CPUCores, humanNum(r.QPS), humanNum(r.ReadsPerSec), humanNum(r.WritesPerSec), truncate(orDash(name), 42))
		}
		t.Flush()
	}
	fmt.Fprintln(b, st.dim("  replica/lease comparisons include live stores within ±25% of median capacity; topology constraints may explain skew"))
	fmt.Fprintln(b)
}

func renderCockroachStorage(b *strings.Builder, st styler, c *model.Context) {
	if c.Server.Engine != "cockroachdb" || c.Health == nil || c.Health.Cockroach == nil {
		return
	}
	s := &c.Health.Cockroach.Storage
	if s.Exactness == "" {
		return
	}
	if !section(b, st, "storage & replication", s.Section) {
		return
	}
	fmt.Fprintf(b, "  %d live stores · filesystem %s used · CockroachDB %s · other use/overhead %s\n",
		s.LiveStores, humanBytes(s.FilesystemUsedBytes), humanBytes(s.CockroachUsedBytes), humanBytes(s.OtherUsedBytes))
	if s.MVCCMetricsAvailable {
		fmt.Fprintf(b, "  MVCC %s live / %s total (%s) · %s garbage\n",
			humanBytes(s.MVCCLiveBytes), humanBytes(s.MVCCTotalBytes), pct(s.MVCCLiveRatio), humanBytes(s.MVCCGarbageBytes))
		fmt.Fprintf(b, "  average MVCC bytes/replica %s–%s (mean %s)\n",
			humanBytes(int64(s.BytesPerReplicaMin)), humanBytes(int64(s.BytesPerReplicaMax)), humanBytes(int64(s.BytesPerReplicaMean)))
	}
	if s.ReplicationMetricsAvailable {
		fmt.Fprintf(b, "  recovery %s uninitialized · %s reserved · %s over-replicated · %s decommissioning\n",
			humanNum(float64(s.UninitializedReplicas)), humanNum(float64(s.ReservedReplicas)),
			humanNum(float64(s.OverreplicatedRanges)), humanNum(float64(s.DecommissioningRanges)))
		fmt.Fprintf(b, "  Raft %s commands pending · max %s on s%d · %s probe / %s snapshot flows\n",
			humanNum(float64(s.RaftCommandsPending)), humanNum(float64(s.MaxRaftCommandsPending)), s.MaxRaftPendingStoreID,
			humanNum(float64(s.RaftProbeFlows)), humanNum(float64(s.RaftSnapshotFlows)))
		fmt.Fprintf(b, "  queues replicate %s pending / %s purgatory · snapshots %s pending\n",
			humanNum(float64(s.ReplicateQueuePending)), humanNum(float64(s.ReplicateQueuePurgatory)), humanNum(float64(s.RaftSnapshotQueuePending)))
	}
	if s.CounterSampledStores > 0 {
		fmt.Fprintf(b, "  %.2fs counter sample on %d/%d stores · %d slow · %d stalled · %.2fs unhealthy · %d write stalls / %.2fs · %d Raft drops\n",
			s.SampleSeconds, s.CounterSampledStores, s.LiveStores, s.DiskSlowEvents, s.DiskStalledEvents,
			s.DiskUnhealthySeconds, s.WriteStallEvents, s.WriteStallSeconds, s.RaftDroppedMessages)
	}
	if s.Reason != "" {
		fmt.Fprintln(b, st.dim("  "+s.Reason))
	}

	stores := append([]model.CockroachStoreStorage(nil), s.Stores...)
	sort.Slice(stores, func(i, j int) bool {
		left, right := storageFilesystemRatio(stores[i]), storageFilesystemRatio(stores[j])
		if left != right {
			return left > right
		}
		return stores[i].StoreID < stores[j].StoreID
	})
	if len(stores) > 0 {
		fmt.Fprintln(b, "\n  STORAGE")
		t := newTab(b)
		fmt.Fprintln(t, "  store\tnode\tstatus\tfilesystem\tCockroachDB\tother/overhead\tMVCC live/total\tbytes/replica")
		for _, store := range stores[:min(30, len(stores))] {
			fmt.Fprintf(t, "  s%d\tn%d\t%s\t%s\t%s\t%s\t%s/%s\t%s\n",
				store.StoreID, store.NodeID, orDash(store.Status), pct(storageFilesystemRatio(store)),
				humanBytes(store.CockroachUsedBytes), humanBytes(store.OtherUsedBytes),
				humanBytes(store.MVCCLiveBytes), humanBytes(store.MVCCTotalBytes), humanBytes(int64(store.BytesPerReplica)))
		}
		t.Flush()

		fmt.Fprintln(b, "\n  REPLICATION")
		t = newTab(b)
		fmt.Fprintln(t, "  store\tnode\treplicas\tuninit\treserved\toverrep\tRaft pending\tprobe/snapshot\treplicate queue\tsnapshot queue")
		for _, store := range stores[:min(30, len(stores))] {
			fmt.Fprintf(t, "  s%d\tn%d\t%d\t%d\t%d\t%d\t%d\t%d/%d\t%d/%d\t%d\n",
				store.StoreID, store.NodeID, store.RangeReplicas, store.UninitializedReplicas, store.ReservedReplicas,
				store.OverreplicatedRanges, store.RaftCommandsPending, store.RaftProbeFlows, store.RaftSnapshotFlows,
				store.ReplicateQueuePending, store.ReplicateQueuePurgatory, store.RaftSnapshotQueuePending)
		}
		t.Flush()
	}

	var eventStores []model.CockroachStoreStorage
	for _, store := range stores {
		if store.DiskSlowEvents > 0 || store.DiskStalledEvents > 0 || store.DiskUnhealthySeconds > 0 ||
			store.WriteStallEvents > 0 || store.WriteStallSeconds > 0 || store.RaftDroppedMessages > 0 {
			eventStores = append(eventStores, store)
		}
	}
	if len(eventStores) > 0 {
		fmt.Fprintln(b, "\n  EVENTS DURING SAMPLE")
		t := newTab(b)
		fmt.Fprintln(t, "  store\tnode\tdisk slow\tdisk stalled\tunhealthy\twrite stalls\twrite-stall time\tRaft drops")
		for _, store := range eventStores {
			fmt.Fprintf(t, "  s%d\tn%d\t%d\t%d\t%.2fs\t%d\t%.2fs\t%d\n",
				store.StoreID, store.NodeID, store.DiskSlowEvents, store.DiskStalledEvents,
				store.DiskUnhealthySeconds, store.WriteStallEvents, store.WriteStallSeconds, store.RaftDroppedMessages)
		}
		t.Flush()
	}
	fmt.Fprintln(b, st.dim("  MVCC bytes are logical replicated bytes; other/overhead is filesystem used minus CockroachDB capacity.used"))
	fmt.Fprintln(b)
}

func storageFilesystemRatio(store model.CockroachStoreStorage) float64 {
	if store.CapacityBytes <= 0 {
		return 0
	}
	return float64(store.FilesystemUsedBytes) / float64(store.CapacityBytes)
}

func renderCockroachJobs(b *strings.Builder, st styler, c *model.Context) {
	if c.Server.Engine != "cockroachdb" || c.Health == nil || c.Health.Cockroach == nil {
		return
	}
	h := c.Health.Cockroach
	if h.Jobs.Exactness == "" {
		return
	}
	if !section(b, st, "jobs & schema changes", h.Jobs) {
		return
	}
	counts := countCockroachJobs(h.JobItems)
	total := h.JobsTotal
	if total == 0 {
		total = len(h.JobItems)
	}
	fmt.Fprintf(b, "  %d relevant · %d active · %d paused · %d reverting · %d failed\n",
		total, counts.active, counts.paused, counts.reverting, counts.failed)
	if h.JobsBounded {
		fmt.Fprintf(b, "%s\n", st.dim(fmt.Sprintf("  showing %d of %d active or recently failed jobs", len(h.JobItems), total)))
	}
	if len(h.JobItems) == 0 {
		fmt.Fprintln(b, st.good("  no active, paused, reverting, or recently failed jobs"))
		fmt.Fprintln(b)
		return
	}
	t := newTab(b)
	fmt.Fprintln(t, "  id\ttype\tstate\tprogress\tage\tlast update\thigh water")
	for _, j := range h.JobItems {
		fmt.Fprintf(t, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			j.JobID, truncate(j.Type, 26), j.State, cockroachJobProgress(j),
			jobAge(c.CollectedAt, j.CreatedAt), ageSince(c.CollectedAt, j.LastUpdatedAt), ageSince(c.CollectedAt, j.HighWaterAt))
	}
	t.Flush()
	for _, j := range h.JobItems[:min(10, len(h.JobItems))] {
		if j.Operation == "" && j.StatusMessage == "" && j.Error == "" {
			continue
		}
		fmt.Fprintf(b, "  job %s · %s\n", j.JobID, truncate(orDash(j.Operation), 108))
		if j.StatusMessage != "" {
			fmt.Fprintf(b, "    status: %s\n", truncate(j.StatusMessage, 104))
		}
		if j.Error != "" {
			fmt.Fprintf(b, "    error: %s\n", truncate(j.Error, 105))
		}
	}
	fmt.Fprintln(b, st.dim("  scope: active jobs plus failures completed in the last 24h; operation, status, and error text are redacted"))
	fmt.Fprintln(b)
}

type cockroachJobCounts struct {
	active, paused, reverting, failed, attention int
}

func countCockroachJobs(items []model.CockroachJobHealth) cockroachJobCounts {
	var counts cockroachJobCounts
	for _, j := range items {
		switch j.State {
		case "failed", "revert-failed":
			counts.failed++
			counts.attention++
		case "paused":
			counts.paused++
			counts.attention++
		case "reverting":
			counts.reverting++
			counts.active++
			counts.attention++
		case "pause-requested", "cancel-requested":
			counts.active++
			counts.attention++
		default:
			counts.active++
		}
	}
	return counts
}

func cockroachJobProgress(j model.CockroachJobHealth) string {
	if !j.ProgressKnown {
		return "—"
	}
	return pct(j.Progress)
}

func jobAge(now, created time.Time) string {
	if created.IsZero() {
		return "unknown"
	}
	return ageSince(now, &created)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
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
	} else if a.LongestActiveSec > 0 {
		fmt.Fprintf(b, "  longest active query %.0fs\n", a.LongestActiveSec)
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
	if c.Server.Engine == "cockroachdb" && c.Queries.Bounded {
		fmt.Fprintln(b, st.dim("  source: bounded SQL Activity top-query cache; lower-ranked fingerprints may be omitted"))
	}
	if len(c.Queries.Top) == 0 {
		empty := "no query activity recorded yet"
		if c.Server.Engine == "cockroachdb" {
			empty = fmt.Sprintf("no persisted statement statistics in the last %dh (CockroachDB flushes them periodically)", c.Queries.WindowHours)
		}
		fmt.Fprintf(b, "  %s\n\n", st.dim(empty))
		return
	}
	tw := newTab(b)
	if c.Server.Engine == "cockroachdb" {
		fmt.Fprintln(tw, "  app\tcalls\ttotal\tmean\tmax p99\tretries\tcontention\tquery")
	} else {
		fmt.Fprintln(tw, "  calls\ttotal\tmean\tquery")
	}
	shown := len(c.Queries.Top)
	if shown > 8 {
		shown = 8
	}
	for _, q := range c.Queries.Top[:shown] {
		if c.Server.Engine == "cockroachdb" {
			fmt.Fprintf(tw, "  %s\t%s\t%s ms\t%.2f ms\t%s\t%d\t%.1f ms\t%s\n",
				orNone(q.AppName), humanNum(float64(q.Calls)), humanNum(q.TotalMS), q.MeanMS,
				formatOptionalMS(q.P99MS, 2), q.MaxRetries, q.ContentionMS, truncate(q.Query, 44))
		} else {
			fmt.Fprintf(tw, "  %s\t%s ms\t%.2f ms\t%s\n", humanNum(float64(q.Calls)), humanNum(q.TotalMS), q.MeanMS, truncate(q.Query, 48))
		}
	}
	tw.Flush()
	if len(c.Queries.Top) > shown {
		fmt.Fprintf(b, "  %s\n", st.dim(fmt.Sprintf("… and %d more (see --json)", len(c.Queries.Top)-shown)))
	}
	fmt.Fprintln(b)
}

func renderCockroachLiveQueries(b *strings.Builder, st styler, c *model.Context) {
	if c.Cockroach == nil {
		return
	}
	live := c.Cockroach.LiveQueries
	if !section(b, st, "live queries", live.Section) {
		return
	}
	if len(live.Items) == 0 {
		fmt.Fprintln(b, "  no user queries running")
		fmt.Fprintln(b)
		return
	}
	tw := newTab(b)
	fmt.Fprintln(tw, "  age\tapp\tphase\tflags\tquery")
	shown := min(15, len(live.Items))
	for _, q := range live.Items[:shown] {
		flags := "—"
		var parts []string
		if q.Distributed {
			parts = append(parts, "distributed")
		}
		if q.FullScan {
			parts = append(parts, "full scan")
		}
		if q.Retries > 0 {
			parts = append(parts, fmt.Sprintf("%d retries", q.Retries))
		}
		if len(parts) > 0 {
			flags = strings.Join(parts, ", ")
		}
		fmt.Fprintf(tw, "  %.0fs\t%s\t%s\t%s\t%s\n", q.AgeSec, orNone(q.AppName), orNone(q.Phase), flags, truncate(q.Query, 48))
	}
	tw.Flush()
	if len(live.Items) > shown {
		fmt.Fprintf(b, "  %s\n", st.dim(fmt.Sprintf("… and %d more running queries (see --json)", len(live.Items)-shown)))
	}
	fmt.Fprintln(b)
}

func renderCockroachInsights(b *strings.Builder, st styler, c *model.Context) {
	if c.Cockroach == nil {
		return
	}
	in := c.Cockroach.ExecutionInsights
	if !section(b, st, "execution insights", in.Section) {
		return
	}
	if len(in.Items) == 0 {
		fmt.Fprintln(b, "  no persisted execution insights")
		fmt.Fprintln(b)
		return
	}
	tw := newTab(b)
	fmt.Fprintln(tw, "  kind\tproblem / causes\tlatency\tretries\tapp\tquery")
	shown := min(12, len(in.Items))
	for _, x := range in.Items[:shown] {
		problem := x.Problem
		if len(x.Causes) > 0 {
			problem += " / " + strings.Join(x.Causes, ",")
		}
		fmt.Fprintf(tw, "  %s\t%s\t%.1f ms\t%d\t%s\t%s\n",
			x.Kind, problem, x.ServiceLatencyMS, x.Retries, orNone(x.AppName), truncate(x.Query, 44))
	}
	tw.Flush()
	if len(in.Items) > shown {
		fmt.Fprintf(b, "  %s\n", st.dim(fmt.Sprintf("… and %d more (see --json)", len(in.Items)-shown)))
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
	if c.Server.Engine == "cockroachdb" {
		renderCockroachTables(b, st, c)
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

func renderCockroachTables(b *strings.Builder, st styler, c *model.Context) {
	tables := c.Tables
	fmt.Fprintf(b, "  %d tables · %s replicated disk estimate\n", tables.Total, humanBytes(tables.DBSizeBytes))
	cacheAge := tableMetadataAge(c)
	coverage := fmt.Sprintf("%d/%d tables scanned", tables.Scanned, tables.Total)
	if tables.MetadataBounded {
		coverage += " (bounded)"
	}
	if cacheAge == "unknown" {
		fmt.Fprintln(b, st.dim("  cached Admin API metadata · age unknown · "+coverage))
	} else {
		fmt.Fprintln(b, st.dim("  cached Admin API metadata · oldest row "+cacheAge+" · "+coverage))
	}
	if len(tables.Top) == 0 {
		fmt.Fprintln(b)
		return
	}
	fmt.Fprintln(b)
	tw := newTab(b)
	fmt.Fprintln(tw, "  table\treplicated\tMVCC live/total\tlive\tranges/repl\tstores\toptimizer stats\ttop hot")
	for _, table := range tables.Top[:min(10, len(tables.Top))] {
		stats := ageSince(c.CollectedAt, table.StatsLastUpdated)
		if !table.AutoStatsEnabled {
			stats += " / auto off"
		}
		if table.MetadataError != "" {
			stats += " / metadata error"
		}
		replication := fmt.Sprintf("%s/%s", humanNum(float64(table.RangeCount)), tableReplicaFactor(table))
		hot := "—"
		if table.TopHotRangeCount > 0 {
			hot = fmt.Sprintf("%d / %s qps", table.TopHotRangeCount, humanNum(table.TopHotRangeQPS))
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s/%s\t%s\t%s\t%d\t%s\t%s\n",
			tableObject(table), humanBytes(table.ReplicatedBytes), humanBytes(table.LiveDataBytes), humanBytes(table.DataBytes),
			tableLivePercent(table), replication, len(table.StoreIDs), stats, hot)
	}
	tw.Flush()
	if len(tables.Top) > 10 {
		fmt.Fprintln(b, st.dim(fmt.Sprintf("  … and %d more (see --json)", len(tables.Top)-10)))
	}
	fmt.Fprintln(b)
}

func tableObject(table model.TableStat) string {
	parts := []string{table.Database, table.Schema, table.Name}
	var qualified []string
	for _, part := range parts {
		if part != "" {
			qualified = append(qualified, part)
		}
	}
	return strings.Join(qualified, ".")
}

func tableMetadataAge(c *model.Context) string {
	if c.Tables == nil {
		return "unknown"
	}
	return ageSince(c.CollectedAt, c.Tables.MetadataOldestAt)
}

func ageSince(now time.Time, at *time.Time) string {
	if at == nil {
		return "unknown"
	}
	if now.IsZero() || now.Before(*at) {
		return at.UTC().Format("2006-01-02")
	}
	return indexUnusedAge(model.IndexStat{UnusedForSeconds: now.Sub(*at).Seconds()}) + " ago"
}

func tableLivePercent(table model.TableStat) string {
	if table.DataBytes <= 0 {
		return "—"
	}
	return pct(table.LiveDataRatio)
}

func tableReplicaFactor(table model.TableStat) string {
	if table.RangeCount <= 0 || table.ReplicaCount <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.1fx", float64(table.ReplicaCount)/float64(table.RangeCount))
}

func renderIndexes(b *strings.Builder, st styler, c *model.Context) {
	if c.Indexes == nil {
		return
	}
	if !section(b, st, "indexes", c.Indexes.Section) {
		return
	}
	if c.Server.Engine == "cockroachdb" {
		renderCockroachIndexes(b, st, c)
		return
	}
	var unusedBytes int64
	for _, ix := range c.Indexes.Unused {
		unusedBytes += ix.Bytes
	}
	fmt.Fprintf(b, "  %d total · %s unused (%s)\n", c.Indexes.Total, humanNum(float64(len(c.Indexes.Unused))), humanBytes(unusedBytes))
	fmt.Fprintln(b)
}

func renderCockroachIndexes(b *strings.Builder, st styler, c *model.Context) {
	idx := c.Indexes
	threshold := indexThreshold(idx)
	fmt.Fprintf(b, "  %d total · %d secondary · %d unused ≥%s\n",
		idx.Total, idx.SecondaryTotal, len(idx.Unused), threshold)
	fmt.Fprintln(b, st.dim("  cluster-wide counters · in-memory and non-durable"))
	if !idx.WriteCountersAvailable {
		fmt.Fprintln(b, st.dim("  write counters unavailable on this CockroachDB version"))
	}
	if idx.Scanned < idx.Total {
		fmt.Fprintf(b, st.dim("  showing a bounded scan of %d indexes\n"), idx.Scanned)
	}
	if len(idx.Unused) > 0 {
		fmt.Fprintln(b)
		fmt.Fprintln(b, st.warn("  UNUSED CANDIDATES — VERIFY BEFORE DROP"))
		tw := newTab(b)
		if idx.WriteCountersAvailable {
			fmt.Fprintln(tw, "  index\treads\twrites\tunused for\tflags")
		} else {
			fmt.Fprintln(tw, "  index\treads\tunused for\tflags")
		}
		for _, ix := range idx.Unused[:min(10, len(idx.Unused))] {
			if idx.WriteCountersAvailable {
				fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", indexObject(ix), humanNum(float64(ix.Scans)), humanNum(float64(ix.Writes)), indexUnusedAge(ix), indexFlags(ix))
			} else {
				fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", indexObject(ix), humanNum(float64(ix.Scans)), indexUnusedAge(ix), indexFlags(ix))
			}
		}
		tw.Flush()
	}
	if len(idx.Usage) > 0 {
		fmt.Fprintln(b)
		fmt.Fprintln(b, "  SECONDARY INDEX USAGE")
		tw := newTab(b)
		if idx.WriteCountersAvailable {
			fmt.Fprintln(tw, "  index\treads\twrites\tlast read\tflags")
		} else {
			fmt.Fprintln(tw, "  index\treads\tlast read\tflags")
		}
		for _, ix := range idx.Usage[:min(10, len(idx.Usage))] {
			lastRead := indexLastRead(c.CollectedAt, ix)
			if idx.WriteCountersAvailable {
				fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", indexObject(ix), humanNum(float64(ix.Scans)), humanNum(float64(ix.Writes)), lastRead, indexFlags(ix))
			} else {
				fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", indexObject(ix), humanNum(float64(ix.Scans)), lastRead, indexFlags(ix))
			}
		}
		tw.Flush()
	}
	fmt.Fprintln(b)
}

func indexObject(ix model.IndexStat) string {
	parts := []string{ix.Database, ix.Schema, ix.Table}
	var qualified []string
	for _, part := range parts {
		if part != "" {
			qualified = append(qualified, part)
		}
	}
	return strings.Join(qualified, ".") + "/" + ix.Name
}

func indexThreshold(idx *model.Indexes) string {
	hours := idx.UnusedThresholdHours
	if hours == 0 {
		hours = 7 * 24
	}
	if hours%24 == 0 {
		return fmt.Sprintf("%dd", hours/24)
	}
	return fmt.Sprintf("%dh", hours)
}

func indexUnusedAge(ix model.IndexStat) string {
	seconds := int64(ix.UnusedForSeconds)
	if seconds >= 24*60*60 {
		return fmt.Sprintf("%.0fd", float64(seconds)/(24*60*60))
	}
	return shortDur(seconds)
}

func indexLastRead(collectedAt time.Time, ix model.IndexStat) string {
	if ix.LastRead == nil {
		if ix.CreatedAt != nil && !collectedAt.IsZero() && collectedAt.After(*ix.CreatedAt) {
			return "never (created " + indexUnusedAge(model.IndexStat{UnusedForSeconds: collectedAt.Sub(*ix.CreatedAt).Seconds()}) + " ago)"
		}
		return "never"
	}
	if collectedAt.IsZero() || collectedAt.Before(*ix.LastRead) {
		return ix.LastRead.UTC().Format("2006-01-02")
	}
	return indexUnusedAge(model.IndexStat{UnusedForSeconds: collectedAt.Sub(*ix.LastRead).Seconds()}) + " ago"
}

func indexFlags(ix model.IndexStat) string {
	var flags []string
	if ix.Unique {
		flags = append(flags, "unique")
	}
	if ix.Inverted {
		flags = append(flags, "inverted")
	}
	if ix.Sharded {
		flags = append(flags, "sharded")
	}
	if ix.Invisible {
		flags = append(flags, "not visible")
	}
	if len(flags) == 0 {
		return "—"
	}
	return strings.Join(flags, ", ")
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

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func formatOptionalMS(v *float64, decimals int) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%.*f ms", decimals, *v)
}

func cockroachQueryHeading(q *model.Queries) string {
	if q == nil || q.WindowHours <= 0 {
		return "TOP QUERIES"
	}
	if q.Bounded {
		return fmt.Sprintf("TOP QUERIES — CACHED %dH", q.WindowHours)
	}
	return fmt.Sprintf("TOP QUERIES — LAST %dH", q.WindowHours)
}

func cockroachQuerySourceNote(q *model.Queries) string {
	if q == nil || q.WindowHours <= 0 {
		return "persisted"
	}
	if q.Bounded {
		return fmt.Sprintf("cached top %dh", q.WindowHours)
	}
	return fmt.Sprintf("persisted %dh", q.WindowHours)
}

func humanBytes2(v float64) string { return humanBytes(int64(v)) }

var _ = time.Now // keep time imported for future use
