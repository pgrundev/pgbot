package monitor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

// FindingState is the identity + severity of one active finding — everything
// the change detector needs and nothing that varies run to run (titles carry
// live numbers like "3.2× slower", so they are display-only, never compared).
type FindingState struct {
	ID       string `json:"id"`
	Object   string `json:"object,omitempty"`
	Severity string `json:"severity"`
	Title    string `json:"title"` // display only; latest wording
}

// Key is the stable identity: finding ID + object. The database fingerprint
// is the outer scope (state is reset when the fingerprint changes).
func (f FindingState) Key() string { return f.ID + "\x00" + f.Object }

// RunRecord summarizes the last SUCCESSFUL diagnostic run.
type RunRecord struct {
	At          time.Time      `json:"at"`
	Fingerprint string         `json:"fingerprint"`
	Database    string         `json:"database"`
	Version     string         `json:"version,omitempty"`
	Score       int            `json:"score"`
	Incomplete  []string       `json:"incomplete,omitempty"` // why the data is partial; empty = complete
	Findings    []FindingState `json:"findings"`             // active (unsuppressed) warn + critical only
	Notes       int            `json:"notes"`                // count of active info findings
}

// ConnFailure tracks a run of consecutive failed checks, separately from the
// last good health state (which a failure must never overwrite).
type ConnFailure struct {
	Since    time.Time `json:"since"`
	Count    int       `json:"count"`
	LastErr  string    `json:"last_error"` // redacted
	Notified bool      `json:"notified"`
}

// Sent records the last notification, so an immediate re-check doesn't post
// the very same banner twice.
type Sent struct {
	At    time.Time `json:"at"`
	Title string    `json:"title"`
	Body  string    `json:"body"`
}

// Activity is one line of the "Recent activity" list in `monitor status`.
type Activity struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"` // ok | warn | crit | fail | info
	Text string    `json:"text"`
}

// State is state.json.
type State struct {
	Version     int          `json:"version"`
	LastAttempt time.Time    `json:"last_attempt,omitempty"`
	LastSuccess *RunRecord   `json:"last_success,omitempty"`
	Failure     *ConnFailure `json:"failure,omitempty"`
	LastSent    *Sent        `json:"last_sent,omitempty"`
	Activity    []Activity   `json:"activity,omitempty"`
}

const (
	stateVersion = 1
	maxActivity  = 8
)

// LoadState reads state.json; a missing file is an empty state. A corrupt
// file is reported (and the caller starts fresh) rather than crashing every
// scheduled run forever.
func LoadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &State{Version: stateVersion}, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return &State{Version: stateVersion}, fmt.Errorf("monitor state is corrupt, starting fresh: %w", err)
	}
	if s.Version == 0 {
		s.Version = stateVersion
	}
	return &s, nil
}

// SaveState writes state.json atomically (0600).
func SaveState(path string, s *State) error {
	s.Version = stateVersion
	if len(s.Activity) > maxActivity {
		s.Activity = s.Activity[len(s.Activity)-maxActivity:]
	}
	return writeJSONAtomic(path, s)
}

func (s *State) log(at time.Time, kind, text string) {
	s.Activity = append(s.Activity, Activity{At: at, Kind: kind, Text: text})
	if len(s.Activity) > maxActivity {
		s.Activity = s.Activity[len(s.Activity)-maxActivity:]
	}
}

// Snapshot reduces an inspected Context to what the monitor keeps. Suppressed
// findings (.pgbot.toml [[ignore]]) and --fail-on-new preexisting ones are
// excluded, exactly as they are excluded from inspect's exit code.
func Snapshot(c *model.Context, score int, incomplete []string) *RunRecord {
	r := &RunRecord{
		At:          c.CollectedAt,
		Fingerprint: c.Fingerprint,
		Database:    c.Server.Database,
		Version:     PGVersion(c.Server.VersionNum),
		Score:       score,
		Incomplete:  incomplete,
		Findings:    []FindingState{},
	}
	seen := map[string]int{}
	for _, f := range c.Findings {
		if f.Suppressed || f.Preexisting {
			continue
		}
		switch f.Severity {
		case model.SeverityCritical, model.SeverityWarn:
		case model.SeverityInfo:
			r.Notes++
			continue
		default:
			continue
		}
		fs := FindingState{ID: f.ID, Object: f.Object, Severity: f.Severity, Title: f.Title}
		// Two findings with one identity: keep the more severe.
		if i, ok := seen[fs.Key()]; ok {
			if rank(fs.Severity) > rank(r.Findings[i].Severity) {
				r.Findings[i] = fs
			}
			continue
		}
		seen[fs.Key()] = len(r.Findings)
		r.Findings = append(r.Findings, fs)
	}
	sortFindings(r.Findings)
	return r
}

// Incomplete lists why a run's data can't support a "healthy" verdict: the
// role can't see the statistics views, or the core health sample came back
// unavailable. A partial run still reports what it found — it just never
// claims everything is fine.
func Incomplete(c *model.Context, hasPgMonitor bool) []string {
	var why []string
	if !hasPgMonitor {
		why = append(why, "role lacks pg_monitor — only partial statistics are visible")
	}
	if c.Health == nil || c.Health.Exactness == model.ExactnessUnavailable {
		why = append(why, "core health metrics were unavailable")
	}
	return why
}

// PGVersion renders server_version_num 180001 as "18.1" (and 90624 as "9.6.24"... unlikely, but not wrong).
func PGVersion(num int) string {
	if num == 0 {
		return ""
	}
	if num >= 100000 {
		return fmt.Sprintf("%d.%d", num/10000, num%10000)
	}
	return fmt.Sprintf("%d.%d.%d", num/10000, (num/100)%100, num%100)
}

func rank(sev string) int {
	switch sev {
	case model.SeverityCritical:
		return 3
	case model.SeverityWarn:
		return 2
	case model.SeverityInfo:
		return 1
	}
	return 0
}

func sortFindings(fs []FindingState) {
	sort.SliceStable(fs, func(i, j int) bool {
		if rank(fs[i].Severity) != rank(fs[j].Severity) {
			return rank(fs[i].Severity) > rank(fs[j].Severity)
		}
		return fs[i].Key() < fs[j].Key()
	})
}

// Counts returns (critical, warn) among active findings.
func (r *RunRecord) Counts() (crit, warn int) {
	if r == nil {
		return 0, 0
	}
	for _, f := range r.Findings {
		switch f.Severity {
		case model.SeverityCritical:
			crit++
		case model.SeverityWarn:
			warn++
		}
	}
	return crit, warn
}
