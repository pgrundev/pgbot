package collect

import (
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

func str(s string) *string { return &s }

func lockSample(pid int32, ev string) WaitSample {
	return WaitSample{PID: pid, State: "active", WaitEventType: str("Lock"), WaitEvent: str(ev)}
}
func cpuSample(pid int32) WaitSample {
	return WaitSample{PID: pid, State: "active"}
}

func edge(victim, holder int, xactAge float64) LockEdge {
	return LockEdge{
		VictimPID: victim, HolderPID: holder, WaitEvent: "transactionid",
		HolderState: "idle in transaction", HolderXactAgeS: xactAge,
		HolderQuery: "SELECT * FROM orders WHERE id = 42 FOR UPDATE",
		HolderUser:  "worker", HolderApp: "worker-7",
		VictimQuery: "UPDATE orders SET status = 'paid' WHERE id = 42",
	}
}

// AAS is samples over SUCCESSFUL polls — failed polls reduce coverage, never
// inflate or deflate the average.
func TestWaitStudyAASAndShares(t *testing.T) {
	in := WaitStudyInput{
		Fast: ashResult{
			samples: []WaitSample{
				lockSample(1, "transactionid"), lockSample(1, "transactionid"),
				lockSample(2, "transactionid"), cpuSample(3),
			},
			attempts: 3, failures: 1, // 2 successful polls
			span: 10 * time.Second,
		},
		HasPgMonitor: true,
	}
	s := BuildWaitStudy(in)
	if s.AAS != 2.0 {
		t.Errorf("AAS = %v, want 2.0 (4 samples / 2 successful polls)", s.AAS)
	}
	if s.Polls != 3 || s.PollFailures != 1 {
		t.Errorf("coverage wrong: %+v", s)
	}
	if s.Profile == nil || !s.Profile.Available {
		t.Fatalf("profile missing: %+v", s.Profile)
	}
	if s.Profile.Buckets[0].Type != "Lock" || s.Profile.Buckets[0].Share != 0.75 {
		t.Errorf("lock bucket wrong: %+v", s.Profile.Buckets)
	}
	if s.Exactness != "sampled" {
		t.Errorf("exactness must be sampled, got %q", s.Exactness)
	}
}

// A blocker is NAMED only with sustained evidence: the same holder seen in ≥3
// lock snapshots. A single observation is reported as transient, never as a
// root cause.
func TestWaitStudyBlockerEvidenceRule(t *testing.T) {
	in := WaitStudyInput{
		Fast: ashResult{samples: []WaitSample{lockSample(18442, "transactionid")}, attempts: 1, span: time.Second},
		Snapshots: []LockSnapshot{
			{Edges: []LockEdge{edge(18442, 8172, 41)}},
			{Edges: []LockEdge{edge(18442, 8172, 42)}},
			{Edges: []LockEdge{edge(18442, 8172, 43), edge(500, 999, 1)}},
		},
		HasPgMonitor: true,
	}
	s := BuildWaitStudy(in)
	if len(s.Blockers) != 1 || s.Blockers[0].HolderPID != 8172 {
		t.Fatalf("want exactly holder 8172 sustained, got %+v", s.Blockers)
	}
	b := s.Blockers[0]
	if b.Observations != 3 || !b.Sustained {
		t.Errorf("sustained evidence wrong: %+v", b)
	}
	if b.HolderXactAgeS != 43 {
		t.Errorf("holder xact age must be the max observed: %v", b.HolderXactAgeS)
	}
	if len(b.Victims) != 1 || b.Victims[0].PID != 18442 {
		t.Errorf("victims wrong: %+v", b.Victims)
	}
	if len(s.Transient) != 1 || s.Transient[0].HolderPID != 999 {
		t.Errorf("single-observation edge must be transient: %+v", s.Transient)
	}
}

// Two observations are enough only when the victim's own sampled time is
// majority Lock — corroboration from the fast plane.
func TestWaitStudyTwoObservationsNeedLockShare(t *testing.T) {
	snapshots := []LockSnapshot{
		{Edges: []LockEdge{edge(18442, 8172, 10)}},
		{Edges: []LockEdge{edge(18442, 8172, 11)}},
	}
	lockHeavy := WaitStudyInput{
		Fast: ashResult{samples: []WaitSample{
			lockSample(18442, "transactionid"), lockSample(18442, "transactionid"), cpuSample(18442),
		}, attempts: 1, span: time.Second},
		Snapshots: snapshots, HasPgMonitor: true,
	}
	if s := BuildWaitStudy(lockHeavy); len(s.Blockers) != 1 || !s.Blockers[0].Sustained {
		t.Errorf("2 observations + majority Lock share must sustain: %+v", s.Blockers)
	}
	cpuHeavy := WaitStudyInput{
		Fast: ashResult{samples: []WaitSample{
			cpuSample(18442), cpuSample(18442), lockSample(18442, "transactionid"),
		}, attempts: 1, span: time.Second},
		Snapshots: snapshots, HasPgMonitor: true,
	}
	if s := BuildWaitStudy(cpuHeavy); len(s.Blockers) != 0 || len(s.Transient) != 1 {
		t.Errorf("2 observations without lock-share corroboration must stay transient: blockers=%+v transient=%+v", s.Blockers, s.Transient)
	}
}

// An idle database is a real result (zero AAS, no blockers), distinct from a
// broken sampler (all polls failed → profile unavailable with a reason).
func TestWaitStudyIdleVsBroken(t *testing.T) {
	idle := BuildWaitStudy(WaitStudyInput{
		Fast: ashResult{attempts: 5, failures: 0, span: time.Second}, HasPgMonitor: true,
	})
	if idle.AAS != 0 || idle.Profile == nil || !idle.Profile.Available || idle.Partial != "" {
		t.Errorf("idle study wrong: %+v", idle)
	}
	broken := BuildWaitStudy(WaitStudyInput{
		Fast: ashResult{attempts: 5, failures: 5, span: time.Second}, HasPgMonitor: true,
	})
	if broken.Profile == nil || broken.Profile.Available || broken.Profile.Reason == "" {
		t.Errorf("broken sampler must be unavailable with a reason: %+v", broken.Profile)
	}
}

// Without pg_monitor only the caller's own sessions are visible — the study
// must say so instead of reporting a quiet server.
func TestWaitStudyPartialWithoutPgMonitor(t *testing.T) {
	s := BuildWaitStudy(WaitStudyInput{
		Fast: ashResult{attempts: 2, span: time.Second},
	})
	if !strings.Contains(s.Partial, "pg_monitor") {
		t.Errorf("missing pg_monitor must be labeled: %q", s.Partial)
	}
}

// --pid narrows the profile and blockers to one backend's story.
func TestWaitStudyFocusPID(t *testing.T) {
	in := WaitStudyInput{
		Fast: ashResult{samples: []WaitSample{
			lockSample(18442, "transactionid"), cpuSample(777), cpuSample(777),
		}, attempts: 1, span: time.Second},
		Snapshots: []LockSnapshot{
			{Edges: []LockEdge{edge(18442, 8172, 1)}}, {Edges: []LockEdge{edge(18442, 8172, 2)}},
			{Edges: []LockEdge{edge(18442, 8172, 3)}}, {Edges: []LockEdge{edge(777, 555, 1)}},
			{Edges: []LockEdge{edge(777, 555, 2)}}, {Edges: []LockEdge{edge(777, 555, 3)}},
		},
		FocusPID: 18442, HasPgMonitor: true,
	}
	s := BuildWaitStudy(in)
	if s.Profile.Samples != 1 {
		t.Errorf("focus must narrow the profile to the PID's samples: %+v", s.Profile)
	}
	if len(s.Blockers) != 1 || s.Blockers[0].HolderPID != 8172 {
		t.Errorf("focus must keep only the PID's blockers: %+v", s.Blockers)
	}
}

// Query text is scrubbed of literals before it enters the report — victim,
// holder, and per-session sample text alike — unless RawQueryText opts out.
func TestWaitStudyScrubsQueryText(t *testing.T) {
	sam := lockSample(18442, "transactionid")
	sam.QueryText = "UPDATE users SET email = 'bob@example.com' WHERE id = 42"
	in := WaitStudyInput{
		Fast: ashResult{samples: []WaitSample{sam}, attempts: 1, span: time.Second},
		Snapshots: []LockSnapshot{
			{Edges: []LockEdge{edge(18442, 8172, 1)}}, {Edges: []LockEdge{edge(18442, 8172, 2)}},
			{Edges: []LockEdge{edge(18442, 8172, 3)}},
		},
		HasPgMonitor: true,
	}
	s := BuildWaitStudy(in)
	all := strings.Join([]string{
		s.Blockers[0].HolderQuery, s.Blockers[0].Victims[0].Query, s.Sessions[0].SampleText,
	}, " ")
	if strings.Contains(all, "bob@example.com") || strings.Contains(all, "42") {
		t.Errorf("literals leaked into the report: %q", all)
	}
	in.RawQueryText = true
	if s := BuildWaitStudy(in); !strings.Contains(s.Sessions[0].SampleText, "bob@example.com") {
		t.Errorf("--raw-query-text must keep the operator's raw view")
	}
}

// Sessions roll up per PID with identity and top wait.
func TestWaitStudySessions(t *testing.T) {
	a := lockSample(1, "transactionid")
	a.Usename, a.Datname, a.AppName = "app", "prod", "web"
	b := cpuSample(1)
	in := WaitStudyInput{
		Fast:         ashResult{samples: []WaitSample{a, a, b, cpuSample(2)}, attempts: 2, span: time.Second},
		HasPgMonitor: true,
	}
	s := BuildWaitStudy(in)
	if len(s.Sessions) != 2 || s.Sessions[0].PID != 1 || s.Sessions[0].Count != 3 {
		t.Fatalf("session rollup wrong: %+v", s.Sessions)
	}
	if s.Sessions[0].TopType != "Lock" || s.Sessions[0].User != "app" || s.Sessions[0].Share != 0.75 {
		t.Errorf("session identity/top wrong: %+v", s.Sessions[0])
	}
}

// Thin samples demote the whole study, reusing the profile's own threshold.
func TestWaitStudyThin(t *testing.T) {
	in := WaitStudyInput{
		Fast:         ashResult{samples: []WaitSample{cpuSample(1)}, attempts: 1, span: time.Second},
		HasPgMonitor: true,
	}
	if s := BuildWaitStudy(in); !s.Thin {
		t.Error("1 sample must be thin")
	}
	many := make([]WaitSample, model.WaitMinSamples)
	for i := range many {
		many[i] = cpuSample(1)
	}
	in.Fast = ashResult{samples: many, attempts: 3, span: time.Second}
	if s := BuildWaitStudy(in); s.Thin {
		t.Error("enough samples must not be thin")
	}
}
