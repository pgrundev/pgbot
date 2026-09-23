package render

import (
	"fmt"
	"io"
	"strings"

	"github.com/pgrundev/pgbot/internal/advisor"
)

// AdvisorInput is everything the index-advisor report needs.
type AdvisorInput struct {
	Color           bool
	Database        string
	VersionNum      int
	Recommendations []advisor.Recommendation
	Considered      int
	Planned         int
	Candidates      int
	SkippedStale    int
}

// AdvisorReport renders validated index recommendations. Every line makes the
// "hypothetical, planner-confirmed, nothing built" guarantee explicit — a user
// must never think pgbot changed their database.
func AdvisorReport(w io.Writer, in AdvisorInput) error {
	st := styler{on: in.Color}
	var b strings.Builder

	fmt.Fprintf(&b, "%s · %s · %s · %s\n\n",
		st.head("index advisor"), st.head(in.Database), pgLower(in.VersionNum),
		st.dim("hypopg validation — nothing was built"))

	if len(in.Recommendations) == 0 {
		fmt.Fprintln(&b, st.good("✓ no validated index recommendations"))
		fmt.Fprintln(&b, st.dim(fmt.Sprintf(
			"considered %d slow quer(y/ies), planned %d, tested %d candidate(s) — the planner didn't confirm a cost win for any.",
			in.Considered, in.Planned, in.Candidates)))
		fmt.Fprintln(&b, st.dim("That's a good sign: the queries pgbot could plan are already served by existing indexes."))
		return writeTextReport(w, b.String())
	}

	fmt.Fprintln(&b, st.head(fmt.Sprintf("%d validated recommendation(s):", len(in.Recommendations))))
	fmt.Fprintln(&b)
	for _, r := range in.Recommendations {
		size := ""
		if r.EstimatedBytes > 0 {
			size = st.dim(fmt.Sprintf("  (~%s)", humanBytes(r.EstimatedBytes)))
		}
		fmt.Fprintf(&b, "%s %s.%s%s\n", st.warn("⚑"), r.Schema, r.Table, size)
		fmt.Fprintf(&b, "  %s;\n", st.head(r.IndexDDL))
		// The query it helps (scrubbed, normalized) and its weight.
		q := oneLineTrunc(r.QueryText, 96)
		fmt.Fprintf(&b, "  %s %s\n", st.dim("helps:"), q)
		fmt.Fprintf(&b, "         %s\n", st.dim(fmt.Sprintf("%s calls · %.0f%% of DB time", humanCount(r.Calls), r.SharePct)))
		// The planner's own verdict — the evidence.
		fmt.Fprintf(&b, "  %s %s\n", st.dim("planner confirmed:"),
			st.good(fmt.Sprintf("cost %s → %s (−%.1f%%)", trimCost(r.CostBefore), trimCost(r.CostAfter), r.ImprovementPct())))
		// Load-bearing caveats render inline, never a footnote.
		for _, cav := range r.Caveats {
			for i, line := range wrapText(cav, 88) {
				prefix := "⚠ but "
				if i > 0 {
					prefix = "      "
				}
				fmt.Fprintf(&b, "  %s\n", st.warn(prefix)+st.dim(line))
			}
		}
		fmt.Fprintf(&b, "  %s\n\n", st.dim("↳ nothing was created."))
	}
	tail := fmt.Sprintf("Validated %d candidate(s) across %d planned quer(y/ies); only planner-confirmed wins are shown.",
		in.Candidates, in.Planned)
	if in.SkippedStale > 0 {
		tail += fmt.Sprintf(" %d candidate(s) skipped: table statistics too stale to trust the estimate (ANALYZE first).", in.SkippedStale)
	}
	fmt.Fprintln(&b, st.dim(tail))
	return writeTextReport(w, b.String())
}

// humanCount abbreviates large call counts (1200000 → 1.2M).
func humanCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// trimCost renders a planner cost without noisy decimals.
func trimCost(c float64) string {
	if c >= 100 {
		return fmt.Sprintf("%.0f", c)
	}
	return fmt.Sprintf("%.1f", c)
}

// oneLineTrunc flattens whitespace and truncates for a single display line.
// Delegates to truncate (terminal.go), which cuts by rune — a byte cut here
// split multibyte SQL text into invalid UTF-8.
func oneLineTrunc(s string, max int) string {
	return truncate(s, max)
}
