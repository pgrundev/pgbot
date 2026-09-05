package render

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pgrundev/pgbot/internal/model"
)

// The default view is a graded, grouped read: a 0–100 health score, then the
// findings bucketed CRITICAL / WARNING / NOTE, then a GOOD list of the healthy
// subsystems named with their values. Fast to read top-to-bottom.

type statusKind int

const (
	kOK statusKind = iota
	kWatch
	kBad
	kInfo
)

func statusColor(st styler, k statusKind) func(string) string {
	switch k {
	case kOK:
		return st.good
	case kWatch:
		return st.warn
	case kBad:
		return st.crit
	default:
		return st.dim
	}
}

func renderGrouped(b *strings.Builder, st styler, c *model.Context, width int) {
	score := computeHealthScore(c)
	paintScore := st.good
	switch {
	case score < 70:
		paintScore = st.crit
	case score < 90:
		paintScore = st.warn
	}
	// Under --profile=schema the score grades only the schema; label it so, and it
	// never reads as an overall "database health" verdict.
	scoreLabel := "Database health:"
	if c.Profile == "schema" {
		scoreLabel = "Schema check:"
	}
	fmt.Fprintf(b, "%s %s\n\n", st.dim(scoreLabel), paintScore(fmt.Sprintf("%d/100", score)))

	var crit, warn, note []model.Finding
	hidden := 0      // suppressed non-criticals: counted in the footer, not listed
	preexisting := 0 // --fail-on-new: already in base, not this change's regressions
	for _, f := range c.Findings {
		// --fail-on-new (D3-2): show only what this change introduced. Preexisting
		// findings drop to a footer count (they remain in --json).
		if f.Preexisting {
			preexisting++
			continue
		}
		// A suppressed CRITICAL still renders (visibly marked) — a config must never
		// be able to make checksum_failures vanish from the screen (B2-2). Lesser
		// suppressed findings drop to the footer / --full.
		if f.Suppressed && f.Severity != model.SeverityCritical {
			hidden++
			continue
		}
		switch f.Severity {
		case model.SeverityCritical:
			crit = append(crit, f)
		case model.SeverityWarn:
			warn = append(warn, f)
		default:
			note = append(note, f)
		}
	}
	byImpact := func(fs []model.Finding) {
		sort.SliceStable(fs, func(i, j int) bool { return fs[i].Impact.Score > fs[j].Impact.Score })
	}
	byImpact(crit)
	byImpact(warn)

	// emit one severity group: a colored header, then a bullet per finding title.
	emit := func(header string, paint func(string) string, fs []model.Finding) {
		if len(fs) == 0 {
			return
		}
		fmt.Fprintln(b, paint(header))
		for _, f := range fs {
			lines := wrapText(f.Title, width-4)
			fmt.Fprintf(b, "%s %s\n", paint("●"), lines[0])
			for _, l := range lines[1:] {
				fmt.Fprintf(b, "  %s\n", l)
			}
			if f.Suppressed { // a rendered-but-suppressed critical
				fmt.Fprintf(b, "  %s\n", st.dim("suppressed by config: "+suppReason(f)))
			}
			// A destructive-action guard travels even in the compact view — a
			// prohibition or precondition is exactly what must not be dropped for space.
			renderSafetyGuards(b, st, f, width, "  ")
		}
		fmt.Fprintln(b)
	}
	emit("CRITICAL", st.crit, crit)
	emit("WARNING", st.warn, warn)
	emit("NOTE", st.info, note)

	if hidden > 0 {
		fmt.Fprintln(b, st.dim(fmt.Sprintf("%d finding(s) suppressed by config (see --full)", hidden)))
		fmt.Fprintln(b)
	}
	if preexisting > 0 {
		fmt.Fprintln(b, st.dim(fmt.Sprintf("%d finding(s) already present in the base — not introduced by this change (see --json)", preexisting)))
		fmt.Fprintln(b)
	}

	// The GOOD list infers health from the ABSENCE of a finding — valid only when
	// the check actually ran. A schema profile skips the workload/infra collectors,
	// so "no blocking locks" there would be a claim about a database it never
	// examined. Suppress it; the header already says this is schema-only.
	if c.Profile != "schema" {
		if good := buildGood(c); len(good) > 0 {
			fmt.Fprintln(b, st.good("GOOD"))
			for _, g := range good {
				fmt.Fprintf(b, "%s %s\n", st.good("●"), st.dim(g))
			}
			fmt.Fprintln(b)
		}
	}

	fmt.Fprintln(b, st.dim("Details: pgbot inspect --full   ·   Machine-readable: --json"))
	fmt.Fprintln(b, st.dim(`Ask it: pgbot ask "what's wrong?"`))
}

// renderSafetyGuards prints a finding's structured destructive-action guards
// compactly — a prohibition as "do NOT <ACTION>", a precondition as "before
// <ACTION>, confirm: <verify>". Guaranteed from the structured field, never
// summary prose, and shared by the grouped view and the --full findings list.
func renderSafetyGuards(b *strings.Builder, st styler, f model.Finding, width int, indent string) {
	if f.Safety == nil {
		return
	}
	for _, g := range f.Safety.BlockingCaveats {
		var line string
		switch {
		case g.Kind == model.GuardProhibition:
			line = "⚠ do NOT " + g.Action + " — " + g.Text
		case g.Verify != nil:
			line = "⚠ before " + g.Action + ", confirm: " + *g.Verify
		default:
			line = "⚠ " + g.Action + ": " + g.Text
		}
		for _, l := range wrapText(line, width-len(indent)-2) {
			fmt.Fprintf(b, "%s%s\n", indent, st.warn(l))
		}
	}
}

// safetyText renders a finding's destructive-action guards as one plain-text line,
// for the text-message surfaces (SARIF, JUnit). Empty when there are no guards.
func safetyText(f *model.Finding) string {
	if f == nil || f.Safety == nil || len(f.Safety.BlockingCaveats) == 0 {
		return ""
	}
	var parts []string
	for _, g := range f.Safety.BlockingCaveats {
		s := "SAFETY [" + g.Action + "]: " + g.Text
		if g.Verify != nil {
			s += " Only after: " + *g.Verify
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// computeHealthScore is a coarse 0–100 grade: full marks minus a penalty per
// finding by severity. It's an at-a-glance indicator, not a precise metric.
// suppReason renders an ignore rule's reason, or a stand-in when none was given.
func suppReason(f model.Finding) string {
	if f.SuppressionReason != "" {
		return f.SuppressionReason
	}
	return "no reason given"
}

// HealthScore exposes the dashboard's 0–100 grade to sibling renderers (the
// HTML report), so every surface shows the same number.
func HealthScore(c *model.Context) int { return computeHealthScore(c) }

func computeHealthScore(c *model.Context) int {
	penalty := 0
	for _, f := range c.Findings {
		if f.Suppressed {
			continue // a muted finding shouldn't drag the grade (B2-2)
		}
		switch f.Severity {
		case model.SeverityCritical:
			penalty += 10
		case model.SeverityWarn:
			penalty += 3
		default:
			penalty += 1
		}
	}
	s := 100 - penalty
	if s < 0 {
		s = 0
	}
	return s
}

// buildGood names the subsystems pgbot checked and found healthy, with their
// values — the "a colleague who looked" signal. Only names things actually
// examined and clean, capped so the list stays scannable.
func buildGood(c *model.Context) []string {
	fired := map[string]bool{}
	for _, f := range c.Findings {
		fired[f.ID] = true
	}
	var g []string
	if h := c.Health; h != nil {
		if h.CacheHitUsable() && !fired["low_cache_hit"] {
			g = append(g, fmt.Sprintf("cache hit ratio %.1f%%", *h.CacheHitRatio*100))
		}
		if h.DeadlocksPerMin != nil && *h.DeadlocksPerMin == 0 {
			g = append(g, "no deadlocks")
		}
	}
	if c.Locks != nil && c.Locks.BlockedCount == 0 {
		g = append(g, "no blocking locks")
	}
	if r := c.Replication; r != nil {
		switch {
		case r.IsReplica:
			g = append(g, "replication healthy (replica)")
		case len(r.Replicas) > 0:
			g = append(g, fmt.Sprintf("replication healthy (%d streaming)", len(r.Replicas)))
		}
	}
	if c.Schema != nil && !fired["index_invalid"] {
		g = append(g, "no invalid indexes")
	}
	if c.Tables != nil && !fired["table_bloat"] {
		g = append(g, "no significant table bloat")
	}
	if c.Limits != nil {
		if !fired["txid_wraparound"] && c.Limits.MaxXIDAge > 0 {
			g = append(g, "no wraparound risk")
		}
		if !fired["connection_saturation"] && c.Limits.ConnectionsMax > 0 {
			g = append(g, fmt.Sprintf("connections %d/%d", c.Limits.ConnectionsUsed, c.Limits.ConnectionsMax))
		}
	}
	if c.Queries != nil && c.Queries.Enabled && !fired["pg_stat_statements_missing"] {
		g = append(g, "query stats available")
	}
	if len(g) > 6 {
		g = g[:6]
	}
	return g
}

// pgLower renders "postgres 16.3" for the header.
func pgLower(num int) string {
	if num == 0 {
		return "postgres"
	}
	return fmt.Sprintf("postgres %d.%d", num/10000, num%100)
}
