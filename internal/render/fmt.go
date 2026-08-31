package render

import (
	"fmt"
	"strings"
	"time"
)

// shortDur formats a second count as a compact human duration (3m12s, 1h4m).
func shortDur(sec int64) string {
	d := time.Duration(sec) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", sec)
	case d < time.Hour:
		return fmt.Sprintf("%dm%ds", sec/60, sec%60)
	default:
		return fmt.Sprintf("%dh%dm", sec/3600, (sec%3600)/60)
	}
}

// humanBytes / humanNum keep the report scannable.
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func humanNum(v float64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("%.1fG", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.1fk", v/1e3)
	case v == float64(int64(v)):
		return fmt.Sprintf("%d", int64(v))
	default:
		return fmt.Sprintf("%.1f", v)
	}
}

func pct(v float64) string { return fmt.Sprintf("%.1f%%", v*100) }

func humanDurationMS(ms float64) string {
	if ms <= 0 {
		return "0s"
	}
	return (time.Duration(ms * float64(time.Millisecond))).Round(time.Millisecond).String()
}

// wrapText word-wraps prose to width columns (for width-adaptive output). An
// empty string yields no lines, so callers emit nothing.
func wrapText(text string, width int) []string {
	if width < 20 {
		width = 20
	}
	var lines []string
	cur := ""
	for _, w := range strings.Fields(text) {
		switch {
		case cur == "":
			cur = w
		case len(cur)+1+len(w) > width:
			lines = append(lines, cur)
			cur = w
		default:
			cur += " " + w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// f2 dereferences an optional float, or "—" when absent.
func f2(p *float64, f func(float64) string) string {
	if p == nil {
		return "—"
	}
	return f(*p)
}

// sparkline renders a float series as Unicode blocks. Empty series -> "".
var sparkChars = []rune("▁▂▃▄▅▆▇█")

func sparkline(vals []float64) string {
	if len(vals) == 0 {
		return ""
	}
	min, max := vals[0], vals[0]
	for _, v := range vals {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	var b strings.Builder
	span := max - min
	for _, v := range vals {
		idx := 0
		if span > 0 {
			idx = int((v - min) / span * float64(len(sparkChars)-1))
		}
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sparkChars) {
			idx = len(sparkChars) - 1
		}
		b.WriteRune(sparkChars[idx])
	}
	return b.String()
}
