package monitor

import (
	"encoding/json"
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("tzdata for %s unavailable: %v", name, err)
	}
	return loc
}

func TestNextDaily(t *testing.T) {
	loc := mustLoc(t, "America/New_York")
	s := Schedule{Kind: Daily, Hour: 9}
	cases := []struct{ now, want time.Time }{
		{time.Date(2026, 3, 10, 8, 59, 0, 0, loc), time.Date(2026, 3, 10, 9, 0, 0, 0, loc)},
		{time.Date(2026, 3, 10, 9, 0, 0, 0, loc), time.Date(2026, 3, 11, 9, 0, 0, 0, loc)}, // exactly now → tomorrow
		{time.Date(2026, 3, 10, 23, 0, 0, 0, loc), time.Date(2026, 3, 11, 9, 0, 0, 0, loc)},
		{time.Date(2026, 12, 31, 10, 0, 0, 0, loc), time.Date(2027, 1, 1, 9, 0, 0, 0, loc)},
	}
	for _, c := range cases {
		got, approx := s.Next(c.now, time.Time{})
		if approx || !got.Equal(c.want) {
			t.Errorf("Next(%v) = %v approx=%v, want %v", c.now, got, approx, c.want)
		}
	}
}

// Across a DST change a daily check stays at 9:00 wall-clock — not 23 or 25
// hours after the previous one landing at 8:00 or 10:00.
func TestNextDailyAcrossDST(t *testing.T) {
	loc := mustLoc(t, "America/New_York")
	s := Schedule{Kind: Daily, Hour: 9}
	for _, now := range []time.Time{
		time.Date(2026, 3, 7, 12, 0, 0, 0, loc),   // day before spring-forward (Mar 8)
		time.Date(2026, 10, 31, 12, 0, 0, 0, loc), // day before fall-back (Nov 1)
	} {
		got, _ := s.Next(now, time.Time{})
		if want := now.AddDate(0, 0, 1); got.Hour() != 9 || got.Minute() != 0 || got.Day() != want.Day() {
			t.Errorf("Next(%v) = %v, want 09:00 the next day", now, got)
		}
	}
}

func TestNextWeekly(t *testing.T) {
	loc := mustLoc(t, "Europe/Berlin")
	s := Schedule{Kind: Weekly, Weekday: time.Monday, Hour: 9, Minute: 30}
	// 2026-11-18 is a Wednesday.
	cases := []struct{ now, want time.Time }{
		{time.Date(2026, 11, 18, 12, 0, 0, 0, loc), time.Date(2026, 11, 23, 9, 30, 0, 0, loc)},
		{time.Date(2026, 11, 23, 9, 0, 0, 0, loc), time.Date(2026, 11, 23, 9, 30, 0, 0, loc)},  // Monday before
		{time.Date(2026, 11, 23, 9, 30, 0, 0, loc), time.Date(2026, 11, 30, 9, 30, 0, 0, loc)}, // Monday at → next week
		{time.Date(2026, 11, 22, 23, 0, 0, 0, loc), time.Date(2026, 11, 23, 9, 30, 0, 0, loc)}, // Sunday night
	}
	for _, c := range cases {
		got, approx := s.Next(c.now, time.Time{})
		if approx || !got.Equal(c.want) {
			t.Errorf("Next(%v) = %v, want %v", c.now, got, c.want)
		}
	}
	sun := Schedule{Kind: Weekly, Weekday: time.Sunday, Hour: 0}
	got, _ := sun.Next(time.Date(2026, 11, 18, 12, 0, 0, 0, loc), time.Time{})
	if want := time.Date(2026, 11, 22, 0, 0, 0, 0, loc); !got.Equal(want) {
		t.Errorf("Sunday midnight: got %v want %v", got, want)
	}
}

func TestNextInterval(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	h := Schedule{Kind: Hourly}
	if got, approx := h.Next(now, time.Time{}); !approx || !got.Equal(now.Add(time.Hour)) {
		t.Errorf("hourly without anchor: %v %v", got, approx)
	}
	if got, _ := h.Next(now, now.Add(-20*time.Minute)); !got.Equal(now.Add(40 * time.Minute)) {
		t.Errorf("hourly anchored 20m ago: %v", got)
	}
	six := Schedule{Kind: Every6h}
	// Anchor 13h ago (missed runs while asleep): next is the next 6h step after now.
	if got, _ := six.Next(now, now.Add(-13*time.Hour)); !got.Equal(now.Add(5 * time.Hour)) {
		t.Errorf("6h anchored 13h ago: %v", got)
	}
	if six.Interval() != 6*time.Hour || h.Interval() != time.Hour || (Schedule{Kind: Daily}).Interval() != 0 {
		t.Error("Interval mapping wrong")
	}
}

func TestValidateSchedule(t *testing.T) {
	good := []Schedule{
		{Kind: Hourly}, {Kind: Every6h}, {Kind: Daily, Hour: 0}, {Kind: Daily, Hour: 23, Minute: 59},
		{Kind: Weekly, Weekday: time.Saturday, Hour: 9},
	}
	for _, s := range good {
		if err := s.Validate(); err != nil {
			t.Errorf("%+v: unexpected %v", s, err)
		}
	}
	bad := []Schedule{
		{}, {Kind: "monthly"}, {Kind: Daily, Hour: 24}, {Kind: Daily, Hour: -1},
		{Kind: Daily, Minute: 60}, {Kind: Weekly, Weekday: 7}, {Kind: Weekly, Weekday: -1},
	}
	for _, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("%+v: expected an error", s)
		}
	}
}

func TestDescribe(t *testing.T) {
	cases := map[string]Schedule{
		"Every hour":               {Kind: Hourly},
		"Every 6 hours":            {Kind: Every6h},
		"Every day at 9:00 AM":     {Kind: Daily, Hour: 9},
		"Every day at 12:05 AM":    {Kind: Daily, Hour: 0, Minute: 5},
		"Every day at 12:00 PM":    {Kind: Daily, Hour: 12},
		"Every Monday at 9:00 AM":  {Kind: Weekly, Weekday: time.Monday, Hour: 9},
		"Every Sunday at 11:30 PM": {Kind: Weekly, Weekday: time.Sunday, Hour: 23, Minute: 30},
	}
	for want, s := range cases {
		if got := s.Describe(); got != want {
			t.Errorf("Describe(%+v) = %q, want %q", s, got, want)
		}
	}
	if got := (Schedule{Kind: Daily, Hour: 9}).TimeLabel(); got != "09:00 AM" {
		t.Errorf("TimeLabel = %q", got)
	}
}

func TestScheduleJSONRoundTrip(t *testing.T) {
	for _, s := range []Schedule{{Kind: Hourly}, {Kind: Weekly, Weekday: time.Friday, Hour: 17, Minute: 45}} {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var back Schedule
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if back != s {
			t.Errorf("round trip %s: got %+v want %+v", b, back, s)
		}
	}
}

func TestParseClock(t *testing.T) {
	ok := map[string][2]int{
		"9": {9, 0}, "9am": {9, 0}, "9 AM": {9, 0}, "09:00 AM": {9, 0}, "9:30pm": {21, 30},
		"21:00": {21, 0}, "0:00": {0, 0}, "12 am": {0, 0}, "12:15 PM": {12, 15}, "12pm": {12, 0},
		"7.30 p.m.": {19, 30}, "  10:05  ": {10, 5}, "11 p": {23, 0},
	}
	for in, want := range ok {
		h, m, err := ParseClock(in)
		if err != nil || h != want[0] || m != want[1] {
			t.Errorf("ParseClock(%q) = %d:%d %v, want %d:%d", in, h, m, err, want[0], want[1])
		}
	}
	for _, in := range []string{"", "24:00", "13pm", "0am", "9:60", "9:5", "noon", "9:00:00", "-1", ":30"} {
		if _, _, err := ParseClock(in); err == nil {
			t.Errorf("ParseClock(%q): expected an error", in)
		}
	}
}

func TestParseWeekday(t *testing.T) {
	ok := map[string]time.Weekday{"monday": time.Monday, "Mon": time.Monday, "SUN": time.Sunday, "1": time.Monday, "7": time.Sunday, "thurs": time.Thursday}
	for in, want := range ok {
		if got, err := ParseWeekday(in); err != nil || got != want {
			t.Errorf("ParseWeekday(%q) = %v %v, want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "mo", "8", "0", "funday"} {
		if _, err := ParseWeekday(in); err == nil {
			t.Errorf("ParseWeekday(%q): expected an error", in)
		}
	}
}

func TestHumanWhen(t *testing.T) {
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC) // Wednesday
	cases := map[string]time.Time{
		"Today at 9:00 AM":      time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC),
		"Tomorrow at 9:00 AM":   time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC),
		"Yesterday at 11:30 PM": time.Date(2026, 11, 17, 23, 30, 0, 0, time.UTC),
		"Monday at 9:00 AM":     time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC),
		"Tue, Dec 1 at 9:00 AM": time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC),
	}
	for want, at := range cases {
		if got := HumanWhen(at, now); got != want {
			t.Errorf("HumanWhen(%v) = %q, want %q", at, got, want)
		}
	}
}
