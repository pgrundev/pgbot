package main

import (
	"strings"
	"testing"
)

// Default view hides plain idle sessions (they're noise on a pooled app) but
// always keeps active, idle-in-transaction, and waiting ones; --all shows
// everything.
func TestActivityKeep(t *testing.T) {
	cases := []struct {
		state, wait string
		all, want   bool
	}{
		{"active", "", false, true},
		{"idle in transaction", "", false, true},
		{"idle in transaction (aborted)", "", false, true},
		{"idle", "", false, false},
		{"idle", "", true, true},
		{"active", "Lock:transactionid", false, true},
	}
	for _, c := range cases {
		if got := keepActivityRow(c.state, c.all); got != c.want {
			t.Errorf("keep(%q, all=%v) = %v, want %v", c.state, c.all, got, c.want)
		}
	}
}

// Ages render short and human: seconds under a minute, then m/h/d.
func TestActivityAge(t *testing.T) {
	cases := map[float64]string{0: "", 3.2: "3s", 43: "43s", 90: "1m30s", 3700: "1h1m", 90000: "1d1h"}
	for in, want := range cases {
		if got := fmtAgeShort(in); got != want {
			t.Errorf("fmtAgeShort(%v) = %q, want %q", in, got, want)
		}
	}
}

// JSON rows are the machine contract: query text scrubbed of literals.
func TestActivityJSONScrubbed(t *testing.T) {
	r := activityRow{PID: 1, State: "active", Query: "SELECT * FROM users WHERE email = 'bob@example.com'"}
	line := activityJSONLine(r)
	if strings.Contains(line, "bob@example.com") {
		t.Errorf("PII leaked: %s", line)
	}
	if !strings.Contains(line, `"pid":1`) || !strings.Contains(line, "users") {
		t.Errorf("structure lost: %s", line)
	}
}
