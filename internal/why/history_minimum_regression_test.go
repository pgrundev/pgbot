package why

import (
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/model"
)

func TestAnalyze_HistoryMinimumForIntervalShift(t *testing.T) {
	samples := history(4, func(i int, c *model.Context) {
		calls := int64(100 * (i + 1))
		totalMS := float64(800)
		if i > 0 {
			totalMS += float64(800 + 2600*(i-1))
		}
		c.Queries = &model.Queries{
			Enabled:     true,
			TotalExecMS: totalMS,
			Top: []model.QueryStat{{
				QueryID: 42,
				Query:   "SELECT * FROM orders WHERE customer_id = $1",
				Calls:   calls,
				TotalMS: totalMS,
			}},
		}
	})

	t.Run("three snapshots are insufficient", func(t *testing.T) {
		report := Analyze(samples[:3], nil, Options{})
		if len(report.Chains) != 0 {
			t.Fatalf("three snapshots produced chains: %+v", report.Chains)
		}
		notes := strings.Join(report.Notes, " ")
		if !strings.Contains(notes, "at least 4") || !strings.Contains(notes, "pgbot inspect") {
			t.Fatalf("notes = %q, want the four-snapshot guidance", notes)
		}
	})

	t.Run("four snapshots detect a sustained change", func(t *testing.T) {
		means, _, _ := queryIntervalMeans(samples, 42)
		if len(means) != 3 {
			t.Fatalf("four snapshots produced %d interval points, want 3", len(means))
		}
		if means[0].Val != 8 || means[1].Val != 26 || means[2].Val != 26 {
			t.Fatalf("interval means = %+v, want 8, 26, 26", means)
		}

		report := Analyze(samples, nil, Options{})
		if len(report.Chains) != 1 {
			t.Fatalf("smallest sufficient history produced %d chains, want 1: %+v", len(report.Chains), report)
		}
		if !strings.Contains(report.Chains[0].Symptom.Text, "slowed 3.2×") {
			t.Fatalf("unexpected detected change: %q", report.Chains[0].Symptom.Text)
		}
	})
}
