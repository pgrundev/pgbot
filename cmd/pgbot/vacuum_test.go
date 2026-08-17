package main

import (
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

func TestExpectAutovacuum(t *testing.T) {
	const th, sc = avDefaultThreshold, avDefaultScaleFactor
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name       string
		live, dead int64
		disabled   bool
		thOver     *float64
		scOver     *float64
		want       bool
	}{
		{"at base threshold", 0, 50, false, nil, nil, false},
		{"one over base", 0, 51, false, nil, nil, true},
		{"under default trigger", 1000, 200, false, nil, nil, false}, // 200 < 50 + 0.2*1000
		{"over default trigger", 1000, 300, false, nil, nil, true},
		{"large under", 100000, 20000, false, nil, nil, false},
		{"large over", 100000, 20100, false, nil, nil, true},
		{"autovacuum disabled is never due", 1000, 999999, true, nil, nil, false},
		{"per-table scale override raises the bar", 1000, 300, false, nil, f(0.5), false}, // 300 < 50 + 0.5*1000
		{"per-table threshold override lowers it", 0, 30, false, f(20), nil, true},        // 30 > 20
	}
	for _, c := range cases {
		ts := model.TableStat{LiveTuples: c.live, DeadTuples: c.dead, AutovacuumDisabled: c.disabled,
			VacuumThresholdOverride: c.thOver, VacuumScaleOverride: c.scOver}
		if got := expectAutovacuum(ts, th, sc); got != c.want {
			t.Errorf("%s: expectAutovacuum(live=%d, dead=%d) = %v, want %v", c.name, c.live, c.dead, got, c.want)
		}
	}
}

func TestAgoStr(t *testing.T) {
	if got := agoStr(nil); got != "never" {
		t.Errorf("nil should be %q, got %q", "never", got)
	}
	var zero time.Time
	if got := agoStr(&zero); got != "never" {
		t.Errorf("zero time should be %q, got %q", "never", got)
	}
	// Values chosen to sit well inside their buckets so sub-second test runtime
	// can't tip them across a boundary.
	m30 := time.Now().Add(-30 * time.Minute)
	if got := agoStr(&m30); got != "30m ago" {
		t.Errorf("30 minutes ago = %q, want %q", got, "30m ago")
	}
	h5 := time.Now().Add(-5 * time.Hour)
	if got := agoStr(&h5); got != "5h ago" {
		t.Errorf("5 hours ago = %q, want %q", got, "5h ago")
	}
	d3 := time.Now().Add(-3 * 24 * time.Hour)
	if got := agoStr(&d3); got != "3d ago" {
		t.Errorf("3 days ago = %q, want %q", got, "3d ago")
	}
}
