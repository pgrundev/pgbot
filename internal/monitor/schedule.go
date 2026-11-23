// Package monitor is PgBot Monitor: scheduled, notification-only health checks
// on the user's own Mac. It owns no diagnostics — every finding, severity and
// score comes from the same engine `pgbot inspect` runs. What lives here is the
// small amount of glue around it: the schedule, the saved preferences, the
// per-run state used to decide what is worth a notification, and the
// macOS-specific adapters (launchd, Keychain, Notification Center).
//
// Everything in this package except the *_darwin.go adapters is portable so it
// can be unit-tested on any OS; the CLI refuses to run the monitor off macOS.
package monitor

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ScheduleKind is one of the four MVP schedules.
type ScheduleKind string

const (
	Hourly    ScheduleKind = "hourly"
	Every6h   ScheduleKind = "every_6_hours"
	Daily     ScheduleKind = "daily"
	Weekly    ScheduleKind = "weekly"
	defaultHr              = 9
)

// Schedule says when launchd starts a check. Hour/Minute/Weekday apply only to
// the calendar kinds (daily, weekly) and are in the Mac's local time zone —
// launchd evaluates StartCalendarInterval against the wall clock, so a 9:00 AM
// check stays at 9:00 AM across daylight-saving changes.
type Schedule struct {
	Kind    ScheduleKind `json:"kind"`
	Hour    int          `json:"hour,omitempty"`    // 0–23
	Minute  int          `json:"minute,omitempty"`  // 0–59
	Weekday time.Weekday `json:"weekday,omitempty"` // weekly only; Sunday=0 … Saturday=6 (launchd uses the same numbering)
}

// DefaultSchedule is the recommended choice: every day at 9:00 AM.
func DefaultSchedule() Schedule { return Schedule{Kind: Daily, Hour: defaultHr} }

// Interval is the StartInterval for the interval kinds, 0 for calendar kinds.
func (s Schedule) Interval() time.Duration {
	switch s.Kind {
	case Hourly:
		return time.Hour
	case Every6h:
		return 6 * time.Hour
	}
	return 0
}

// Calendar reports whether launchd should use StartCalendarInterval.
func (s Schedule) Calendar() bool { return s.Kind == Daily || s.Kind == Weekly }

// Validate rejects anything outside the four supported schedules.
func (s Schedule) Validate() error {
	switch s.Kind {
	case Hourly, Every6h:
		return nil
	case Daily, Weekly:
		if s.Hour < 0 || s.Hour > 23 {
			return fmt.Errorf("hour must be 0–23, got %d", s.Hour)
		}
		if s.Minute < 0 || s.Minute > 59 {
			return fmt.Errorf("minute must be 0–59, got %d", s.Minute)
		}
		if s.Kind == Weekly && (s.Weekday < time.Sunday || s.Weekday > time.Saturday) {
			return fmt.Errorf("weekday must be Sunday–Saturday, got %d", int(s.Weekday))
		}
		return nil
	case "":
		return fmt.Errorf("schedule is missing")
	}
	return fmt.Errorf("unsupported schedule %q (hourly, every_6_hours, daily, weekly)", s.Kind)
}

// Describe renders the schedule as a sentence: "Every day at 9:00 AM".
func (s Schedule) Describe() string {
	switch s.Kind {
	case Hourly:
		return "Every hour"
	case Every6h:
		return "Every 6 hours"
	case Daily:
		return "Every day at " + FormatClock(s.Hour, s.Minute)
	case Weekly:
		return "Every " + s.Weekday.String() + " at " + FormatClock(s.Hour, s.Minute)
	}
	return "Unknown schedule"
}

// FrequencyLabel / TimeLabel split Describe for the confirmation table.
func (s Schedule) FrequencyLabel() string {
	switch s.Kind {
	case Daily:
		return "Every day"
	case Weekly:
		return "Every " + s.Weekday.String()
	}
	return s.Describe()
}

func (s Schedule) TimeLabel() string {
	if !s.Calendar() {
		return ""
	}
	return fmt.Sprintf("%02d:%02d %s", hour12(s.Hour), s.Minute, ampm(s.Hour))
}

// Next is the next time launchd will start a check after now.
//
// For calendar schedules this is exact (in now's location). For interval
// schedules launchd counts from when the job was loaded and skips intervals
// missed during sleep, so the answer is an estimate anchored on the last attempt
// (or the install time); approx is true and callers should say "about".
func (s Schedule) Next(now, anchor time.Time) (next time.Time, approx bool) {
	if iv := s.Interval(); iv > 0 {
		if anchor.IsZero() {
			return now.Add(iv), true
		}
		n := anchor.Add(iv)
		for !n.After(now) {
			n = n.Add(iv)
		}
		return n, true
	}
	loc := now.Location()
	cand := time.Date(now.Year(), now.Month(), now.Day(), s.Hour, s.Minute, 0, 0, loc)
	if s.Kind == Weekly {
		days := (int(s.Weekday) - int(cand.Weekday()) + 7) % 7
		cand = time.Date(cand.Year(), cand.Month(), cand.Day()+days, s.Hour, s.Minute, 0, 0, loc)
		if !cand.After(now) {
			cand = time.Date(cand.Year(), cand.Month(), cand.Day()+7, s.Hour, s.Minute, 0, 0, loc)
		}
		return cand, false
	}
	if !cand.After(now) {
		cand = time.Date(cand.Year(), cand.Month(), cand.Day()+1, s.Hour, s.Minute, 0, 0, loc)
	}
	return cand, false
}

// FormatClock renders 9,0 as "9:00 AM".
func FormatClock(h, m int) string { return fmt.Sprintf("%d:%02d %s", hour12(h), m, ampm(h)) }

func hour12(h int) int {
	if h%12 == 0 {
		return 12
	}
	return h % 12
}

func ampm(h int) string {
	if h < 12 {
		return "AM"
	}
	return "PM"
}

// ParseClock accepts the time formats people actually type: "9", "9am",
// "9:30", "09:30 PM", "21:00", "12 am". It returns 24-hour hour and minute.
func ParseClock(in string) (hour, minute int, err error) {
	s := strings.ToLower(strings.Join(strings.Fields(in), ""))
	s = strings.NewReplacer("a.m.", "am", "p.m.", "pm", "a.m", "am", "p.m", "pm").Replace(s)
	s = strings.ReplaceAll(s, ".", ":") // 7.30 → 7:30
	if s == "" {
		return 0, 0, fmt.Errorf("enter a time like 9:00 AM or 21:00")
	}
	suffix := ""
	for _, suf := range []string{"am", "pm", "a", "p"} {
		if strings.HasSuffix(s, suf) {
			suffix = suf[:1]
			s = strings.TrimSuffix(s, suf)
			break
		}
	}
	hs, ms, hasColon := strings.Cut(s, ":")
	if !hasColon {
		ms = "0"
	}
	h, err1 := strconv.Atoi(hs)
	m, err2 := strconv.Atoi(ms)
	if err1 != nil || err2 != nil || hs == "" || (hasColon && len(ms) != 2) {
		return 0, 0, fmt.Errorf("%q is not a time — try 9:00 AM or 21:00", in)
	}
	if m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("minutes must be 00–59")
	}
	switch suffix {
	case "a", "p":
		if h < 1 || h > 12 {
			return 0, 0, fmt.Errorf("with AM/PM the hour must be 1–12")
		}
		h %= 12
		if suffix == "p" {
			h += 12
		}
	default:
		if h < 0 || h > 23 {
			return 0, 0, fmt.Errorf("the hour must be 0–23")
		}
	}
	return h, m, nil
}

// ParseWeekday accepts a full or three-letter English day name, or 1–7 with
// Monday=1 (the order the wizard lists them).
func ParseWeekday(in string) (time.Weekday, error) {
	s := strings.ToLower(strings.TrimSpace(in))
	if n, err := strconv.Atoi(s); err == nil {
		if n < 1 || n > 7 {
			return 0, fmt.Errorf("pick 1–7")
		}
		return time.Weekday(n % 7), nil
	}
	for d := time.Sunday; d <= time.Saturday; d++ {
		name := strings.ToLower(d.String())
		if len(s) >= 3 && strings.HasPrefix(name, s) {
			return d, nil
		}
	}
	return 0, fmt.Errorf("%q is not a day of the week", in)
}

// HumanWhen renders t relative to now: "Today at 9:00 AM", "Tomorrow at …",
// "Monday at …" within a week, else "Mon, Jan 2 at …".
func HumanWhen(t, now time.Time) string {
	t = t.In(now.Location())
	clock := FormatClock(t.Hour(), t.Minute())
	y1, m1, d1 := now.Date()
	y2, m2, d2 := t.Date()
	day0 := time.Date(y1, m1, d1, 0, 0, 0, 0, now.Location())
	day1 := time.Date(y2, m2, d2, 0, 0, 0, 0, now.Location())
	days := int(day1.Sub(day0).Round(time.Hour).Hours() / 24)
	switch {
	case days == 0:
		return "Today at " + clock
	case days == 1:
		return "Tomorrow at " + clock
	case days == -1:
		return "Yesterday at " + clock
	case days > 1 && days < 7:
		return t.Weekday().String() + " at " + clock
	}
	return t.Format("Mon, Jan 2") + " at " + clock
}
