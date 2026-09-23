package render

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

// DiffInput is everything the `pgbot diff` report needs.
type DiffInput struct {
	Color       bool
	Database    string
	Fingerprint string
	BaselineAt  time.Time
	CurrentAt   time.Time
	Requested   time.Duration // --since
	Actual      time.Duration // CurrentAt - BaselineAt (>= Requested)
	ResetReason string        // non-empty: stats reset between snapshots → all deltas fiction
	PgssEvicted bool          // pg_stat_statements evicted between snapshots → query deltas incomplete
	Deltas      *model.Deltas
}

// DiffReport renders a comparison of two baseline snapshots. It never claims the
// requested interval when it got a different one, and it says up front when a
// reset or eviction between the snapshots makes specific deltas untrustworthy.
func DiffReport(w io.Writer, in DiffInput) error {
	st := styler{on: in.Color}
	var b strings.Builder

	target := in.Database
	if target == "" {
		target = shortFP(in.Fingerprint)
	}
	fmt.Fprintf(&b, "%s · %s · %s\n", st.head("diff"), st.head(target), st.dim(shortFP(in.Fingerprint)))

	// (1) Print the interval actually compared, not the one requested. The baseline
	// is the newest snapshot at least --since old, so Actual >= Requested; when the
	// nearest is materially older, say so — a silent substitution turns a
	// comparison into a lie.
	fmt.Fprintf(&b, "%s → %s  ·  %s elapsed\n",
		in.BaselineAt.Format("2006-01-02 15:04"), in.CurrentAt.Format("2006-01-02 15:04"), st.head(HumanDur(in.Actual)))
	if in.Actual > in.Requested+in.Requested/4 {
		fmt.Fprintf(&b, "%s\n", st.warn(fmt.Sprintf(
			"note: you asked for ~%s back, but the nearest older snapshot is %s back — comparing that.",
			HumanDur(in.Requested), HumanDur(in.Actual))))
	}
	fmt.Fprintln(&b)

	// (2) Reset / eviction suppression, reused from the same logic inspect uses.
	if in.ResetReason != "" {
		fmt.Fprintln(&b, st.crit("⚠ statistics were reset between these snapshots — "+in.ResetReason))
		fmt.Fprintln(&b, st.dim("  Cumulative deltas are meaningless across a reset; only same-moment gauges below are trustworthy."))
		fmt.Fprintln(&b)
	}
	if in.PgssEvicted {
		fmt.Fprintln(&b, st.warn("⚠ pg_stat_statements evicted entries between these snapshots — query-level deltas may be incomplete"))
		fmt.Fprintln(&b, st.dim("  A query that fell out of the top set looks 'gone' here even if it's still running."))
		fmt.Fprintln(&b)
	}

	if in.Deltas == nil || len(in.Deltas.Changes) == 0 {
		fmt.Fprintln(&b, st.good("✓ nothing material changed between these snapshots"))
		return writeTextReport(w, b.String())
	}

	fmt.Fprintln(&b, st.head(fmt.Sprintf("%d change(s):", len(in.Deltas.Changes))))
	for _, d := range in.Deltas.Changes {
		color := st.info
		if d.Severity == model.SeverityWarn {
			color = st.warn
		}
		change := fmt.Sprintf("%s → %s", humanNum(d.Before), humanNum(d.After))
		if d.PctChange != nil {
			change += fmt.Sprintf(" (%+.0f%%)", *d.PctChange*100)
		}
		fmt.Fprintf(&b, "  %s %s  %s  %s\n", color("·"), d.Subject, change, st.dim(d.Note))
	}
	return writeTextReport(w, b.String())
}

func writeTextReport(w io.Writer, text string) error {
	n, err := io.WriteString(w, text)
	if err != nil {
		return err
	}
	if n != len(text) {
		return io.ErrShortWrite
	}
	return nil
}

// shortFP abbreviates a fingerprint for display.
func shortFP(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}

// HumanDur renders a duration like "31h" / "2d4h" / "45m".
func HumanDur(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}
