package monitor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

var t0 = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

func fs(id, obj, sev, title string) FindingState {
	return FindingState{ID: id, Object: obj, Severity: sev, Title: title}
}

func run(at time.Time, score int, f ...FindingState) *RunRecord {
	return &RunRecord{At: at, Fingerprint: "fp1", Database: "production", Score: score, Findings: f}
}

func ok(at time.Time, r *RunRecord) Outcome { return Outcome{At: at, Run: r} }

func fail(at time.Time) Outcome {
	return Outcome{At: at, Err: errors.New("connect: dial tcp 10.0.0.5:5432: connection refused")}
}

// step runs Decide and returns the decision; state threads through.
func step(t *testing.T, p Policy, st **State, out Outcome) Decision {
	t.Helper()
	d := Decide(p, "production", *st, out)
	*st = d.State
	return d
}

func empty() *State { return &State{Version: stateVersion} }

var (
	slow   = fs("slow_queries", "q:1", model.SeverityWarn, "Queries are 3.2× slower")
	conns  = fs("connection_saturation", "", model.SeverityWarn, "Connection usage reached 87%")
	xid    = fs("xid_wraparound", "db:production", model.SeverityCritical, "Transaction ID age is approaching wraparound")
	xidWrn = fs("xid_wraparound", "db:production", model.SeverityWarn, "Transaction ID age is high")
)

func TestFirstRunHealthy(t *testing.T) {
	st := empty()
	d := step(t, PolicyImportant, &st, ok(t0, run(t0, 98)))
	if d.Notify == nil || d.Notify.Title != "🐘 PgBot · Production is healthy" {
		t.Fatalf("first healthy run: %+v", d.Notify)
	}
	if !strings.Contains(d.Notify.Body, "98/100") {
		t.Errorf("body lacks score: %q", d.Notify.Body)
	}
}

func TestFirstRunWithProblems(t *testing.T) {
	st := empty()
	d := step(t, PolicyImportant, &st, ok(t0, run(t0, 48, xid, slow)))
	if d.Notify == nil || d.Notify.Kind != "crit" || !strings.HasPrefix(d.Notify.Title, "🚨") {
		t.Fatalf("first run with a critical: %+v", d.Notify)
	}
	if !strings.Contains(d.Notify.Body, "wraparound") || !strings.Contains(d.Notify.Body, "48/100") {
		t.Errorf("body: %q", d.Notify.Body)
	}
}

func TestSameFindingsAreQuiet(t *testing.T) {
	st := empty()
	step(t, PolicyImportant, &st, ok(t0, run(t0, 94, slow)))
	// The title's live number changed; identity didn't — still no notification.
	moved := slow
	moved.Title = "Queries are 4.1× slower"
	d := step(t, PolicyImportant, &st, ok(t0.Add(24*time.Hour), run(t0.Add(24*time.Hour), 94, moved)))
	if d.Notify != nil {
		t.Fatalf("unchanged finding re-notified: %+v", d.Notify)
	}
	if len(d.Changes.Unchanged) != 1 {
		t.Errorf("changes: %+v", d.Changes)
	}
}

func TestNewWarningNotifiesOnce(t *testing.T) {
	st := empty()
	step(t, PolicyImportant, &st, ok(t0, run(t0, 100)))
	t1 := t0.Add(24 * time.Hour)
	d := step(t, PolicyImportant, &st, ok(t1, run(t1, 94, slow, conns)))
	if d.Notify == nil || d.Notify.Kind != "warn" || !strings.HasPrefix(d.Notify.Title, "⚠️ PgBot") {
		t.Fatalf("new warnings: %+v", d.Notify)
	}
	for _, want := range []string{"Health 94/100", "Queries are 3.2× slower", "Connection usage reached 87%"} {
		if !strings.Contains(d.Notify.Body, want) {
			t.Errorf("body %q lacks %q", d.Notify.Body, want)
		}
	}
	t2 := t1.Add(24 * time.Hour)
	if d := step(t, PolicyImportant, &st, ok(t2, run(t2, 94, slow, conns))); d.Notify != nil {
		t.Errorf("second identical run notified: %+v", d.Notify)
	}
}

func TestSameIDDifferentObjectIsNew(t *testing.T) {
	st := empty()
	a := fs("bloated_table", "public.a", model.SeverityWarn, "Table bloat")
	b := fs("bloated_table", "public.b", model.SeverityWarn, "Table bloat")
	step(t, PolicyImportant, &st, ok(t0, run(t0, 97, a)))
	d := step(t, PolicyImportant, &st, ok(t0.Add(time.Hour), run(t0.Add(time.Hour), 94, a, b)))
	if len(d.Changes.New) != 1 || d.Changes.New[0].Object != "public.b" || d.Notify == nil {
		t.Fatalf("expected public.b as new: %+v / %+v", d.Changes, d.Notify)
	}
}

func TestEscalationToCritical(t *testing.T) {
	for _, p := range []Policy{PolicyImportant, PolicyCritical} {
		st := empty()
		step(t, p, &st, ok(t0, run(t0, 97, xidWrn)))
		d := step(t, p, &st, ok(t0.Add(time.Hour), run(t0.Add(time.Hour), 90, xid)))
		if d.Notify == nil || d.Notify.Kind != "crit" || len(d.Changes.Escalated) != 1 {
			t.Fatalf("%s: escalation: %+v %+v", p, d.Notify, d.Changes)
		}
		if !strings.HasSuffix(d.Notify.Body, "Review immediately.") {
			t.Errorf("critical body: %q", d.Notify.Body)
		}
	}
}

func TestRecovery(t *testing.T) {
	st := empty()
	step(t, PolicyImportant, &st, ok(t0, run(t0, 72, slow)))
	d := step(t, PolicyImportant, &st, ok(t0.Add(time.Hour), run(t0.Add(time.Hour), 96)))
	if d.Notify == nil || d.Notify.Title != "✅ PgBot · Production recovered" {
		t.Fatalf("recovery: %+v", d.Notify)
	}
	if !strings.Contains(d.Notify.Body, "improved from 72 to 96") {
		t.Errorf("body: %q", d.Notify.Body)
	}
	if len(d.Changes.Resolved) != 1 {
		t.Errorf("resolved: %+v", d.Changes)
	}
}

func TestPartialRecoveryIsImproving(t *testing.T) {
	st := empty()
	step(t, PolicyImportant, &st, ok(t0, run(t0, 91, slow, conns)))
	d := step(t, PolicyImportant, &st, ok(t0.Add(time.Hour), run(t0.Add(time.Hour), 94, conns)))
	if d.Notify == nil || !strings.Contains(d.Notify.Title, "improving") || !strings.Contains(d.Notify.Body, "1 issue remains") {
		t.Fatalf("partial recovery: %+v", d.Notify)
	}
}

func TestCriticalPolicyIgnoresWarnings(t *testing.T) {
	st := empty()
	step(t, PolicyCritical, &st, ok(t0, run(t0, 100)))
	d := step(t, PolicyCritical, &st, ok(t0.Add(time.Hour), run(t0.Add(time.Hour), 94, slow, conns)))
	if d.Notify != nil {
		t.Fatalf("critical-only notified on warnings: %+v", d.Notify)
	}
	// A warning going away isn't a recovery from a critical either.
	d = step(t, PolicyCritical, &st, ok(t0.Add(2*time.Hour), run(t0.Add(2*time.Hour), 97, conns)))
	if d.Notify != nil {
		t.Fatalf("critical-only notified a warning recovery: %+v", d.Notify)
	}
	d = step(t, PolicyCritical, &st, ok(t0.Add(3*time.Hour), run(t0.Add(3*time.Hour), 87, conns, xid)))
	if d.Notify == nil || d.Notify.Kind != "crit" {
		t.Fatalf("critical-only missed a new critical: %+v", d.Notify)
	}
	d = step(t, PolicyCritical, &st, ok(t0.Add(4*time.Hour), run(t0.Add(4*time.Hour), 97, conns)))
	if d.Notify == nil || d.Notify.Kind != "ok" {
		t.Fatalf("critical-only missed the critical's recovery: %+v", d.Notify)
	}
}

func TestEveryPolicyAlwaysSummarizes(t *testing.T) {
	st := empty()
	step(t, PolicyEvery, &st, ok(t0, run(t0, 98)))
	d := step(t, PolicyEvery, &st, ok(t0.Add(time.Hour), run(t0.Add(time.Hour), 98)))
	if d.Notify == nil || !strings.Contains(d.Notify.Title, "is healthy") || !strings.Contains(d.Notify.Body, "No new issues") {
		t.Fatalf("every-check healthy summary: %+v", d.Notify)
	}
	step(t, PolicyEvery, &st, ok(t0.Add(2*time.Hour), run(t0.Add(2*time.Hour), 97, slow)))
	d = step(t, PolicyEvery, &st, ok(t0.Add(3*time.Hour), run(t0.Add(3*time.Hour), 97, slow)))
	if d.Notify == nil || !strings.HasSuffix(d.Notify.Body, "(unchanged)") {
		t.Fatalf("every-check unchanged warning: %+v", d.Notify)
	}
	// Failures notify on every attempt under "every".
	step(t, PolicyEvery, &st, fail(t0.Add(4*time.Hour)))
	if d := step(t, PolicyEvery, &st, fail(t0.Add(5*time.Hour))); d.Notify == nil {
		t.Fatal("every-check should surface each failed check")
	}
}

func TestConnectionFailureTransitions(t *testing.T) {
	st := empty()
	step(t, PolicyImportant, &st, ok(t0, run(t0, 94, slow)))
	good := st.LastSuccess

	d := step(t, PolicyImportant, &st, fail(t0.Add(time.Hour)))
	if d.Notify == nil || d.Notify.Title != "🔴 PgBot · Connection failed" || !strings.Contains(d.Notify.Body, "Cannot reach Production") {
		t.Fatalf("first failure: %+v", d.Notify)
	}
	if st.LastSuccess != good {
		t.Fatal("a failure overwrote the last successful health state")
	}
	if st.Failure == nil || !st.Failure.Notified || st.Failure.Count != 1 {
		t.Fatalf("failure state: %+v", st.Failure)
	}

	for i := 2; i <= 4; i++ {
		if d := step(t, PolicyImportant, &st, fail(t0.Add(time.Duration(i)*time.Hour))); d.Notify != nil {
			t.Fatalf("failure #%d re-notified: %+v", i, d.Notify)
		}
	}
	if st.Failure.Count != 4 {
		t.Errorf("count = %d", st.Failure.Count)
	}

	d = step(t, PolicyImportant, &st, ok(t0.Add(5*time.Hour), run(t0.Add(5*time.Hour), 94, slow)))
	if d.Notify == nil || d.Notify.Title != "✅ PgBot · Connection restored" {
		t.Fatalf("restore: %+v", d.Notify)
	}
	if st.Failure != nil {
		t.Error("failure not cleared on success")
	}
	// The finding set was compared with the pre-outage run: nothing new.
	if len(d.Changes.New) != 0 {
		t.Errorf("outage produced phantom new findings: %+v", d.Changes.New)
	}
	// Critical-only users are still told about connectivity.
	st2 := empty()
	step(t, PolicyCritical, &st2, ok(t0, run(t0, 100)))
	if d := step(t, PolicyCritical, &st2, fail(t0.Add(time.Hour))); d.Notify == nil {
		t.Error("critical policy must notify a connection failure")
	}
}

func TestNewProblemsOutrankRestored(t *testing.T) {
	st := empty()
	step(t, PolicyImportant, &st, ok(t0, run(t0, 100)))
	step(t, PolicyImportant, &st, fail(t0.Add(time.Hour)))
	d := step(t, PolicyImportant, &st, ok(t0.Add(2*time.Hour), run(t0.Add(2*time.Hour), 90, xid)))
	if d.Notify == nil || d.Notify.Kind != "crit" {
		t.Fatalf("a new critical after an outage must lead: %+v", d.Notify)
	}
}

func TestFingerprintChangeStartsOver(t *testing.T) {
	st := empty()
	step(t, PolicyImportant, &st, ok(t0, run(t0, 94, slow)))
	other := run(t0.Add(time.Hour), 100)
	other.Fingerprint = "fp2"
	d := step(t, PolicyImportant, &st, ok(t0.Add(time.Hour), other))
	if len(d.Changes.Resolved) != 0 || d.Notify == nil || !strings.Contains(d.Notify.Title, "healthy") {
		t.Fatalf("another database must not 'recover' the old one's findings: %+v %+v", d.Changes, d.Notify)
	}
}

func TestIncompleteNeverHealthy(t *testing.T) {
	st := empty()
	r := run(t0, 100)
	r.Incomplete = []string{"role lacks pg_monitor — only partial statistics are visible"}
	d := step(t, PolicyEvery, &st, ok(t0, r))
	if d.Notify == nil || strings.Contains(strings.ToLower(d.Notify.Title+d.Notify.Body), "healthy") {
		t.Fatalf("partial data claimed healthy: %+v", d.Notify)
	}
	if !strings.Contains(d.Notify.Title, "partial data") {
		t.Errorf("title: %q", d.Notify.Title)
	}
}

func TestDuplicateSuppression(t *testing.T) {
	st := empty()
	step(t, PolicyEvery, &st, ok(t0, run(t0, 98)))
	step(t, PolicyEvery, &st, ok(t0.Add(time.Hour), run(t0.Add(time.Hour), 98)))
	// `monitor now` one minute after the scheduled run: identical banner, suppressed.
	d := step(t, PolicyEvery, &st, ok(t0.Add(time.Hour+time.Minute), run(t0.Add(time.Hour+time.Minute), 98)))
	if d.Notify != nil {
		t.Fatalf("identical banner within the dedupe window: %+v", d.Notify)
	}
	d = step(t, PolicyEvery, &st, ok(t0.Add(2*time.Hour), run(t0.Add(2*time.Hour), 98)))
	if d.Notify == nil {
		t.Fatal("dedupe must expire")
	}
}

func TestNotificationTruncation(t *testing.T) {
	long := strings.Repeat("very ", 80) + "long title"
	st := empty()
	d := step(t, PolicyImportant, &st, ok(t0, run(t0, 40,
		fs("a", "", model.SeverityCritical, long), fs("b", "", model.SeverityWarn, "Second"), fs("c", "", model.SeverityWarn, "Third"))))
	if n := len([]rune(d.Notify.Body)); n > maxBody {
		t.Errorf("body %d runes > %d", n, maxBody)
	}
	if n := len([]rune(d.Notify.Title)); n > maxTitle {
		t.Errorf("title %d runes > %d", n, maxTitle)
	}
	st = empty()
	d = step(t, PolicyImportant, &st, ok(t0, run(t0, 40, xid, slow, conns)))
	if !strings.Contains(d.Notify.Body, "+1 more") || strings.Contains(d.Notify.Body, "Connection usage") {
		t.Errorf("at most two findings plus a count: %q", d.Notify.Body)
	}
}

func TestFailureTextIsRedacted(t *testing.T) {
	st := empty()
	d := step(t, PolicyImportant, &st, Outcome{At: t0, Err: errors.New("connect postgres://app:hunter2@db:5432/prod failed; password=hunter2")})
	for _, s := range append(d.Summary, st.Failure.LastErr, d.Notify.Body, d.Notify.Title) {
		if strings.Contains(s, "hunter2") {
			t.Fatalf("secret leaked: %q", s)
		}
	}
}

func TestActivityBounded(t *testing.T) {
	st := empty()
	for i := 0; i < 30; i++ {
		step(t, PolicyImportant, &st, ok(t0.Add(time.Duration(i)*time.Hour), run(t0.Add(time.Duration(i)*time.Hour), 100)))
	}
	if len(st.Activity) > maxActivity {
		t.Errorf("activity grew to %d", len(st.Activity))
	}
}

func TestSnapshotExcludesSuppressedAndInfo(t *testing.T) {
	c := &model.Context{CollectedAt: t0, Fingerprint: "fp", Server: model.ServerInfo{Database: "prod", VersionNum: 180001}}
	c.Findings = []model.Finding{
		{ID: "a", Severity: model.SeverityWarn, Title: "A"},
		{ID: "b", Severity: model.SeverityCritical, Title: "B", Suppressed: true},
		{ID: "c", Severity: model.SeverityInfo, Title: "C"},
		{ID: "d", Object: "x", Severity: model.SeverityWarn, Title: "D1"},
		{ID: "d", Object: "x", Severity: model.SeverityCritical, Title: "D2"},
		{ID: "e", Severity: model.SeverityWarn, Preexisting: true},
	}
	r := Snapshot(c, 80, nil)
	if len(r.Findings) != 2 || r.Notes != 1 || r.Version != "18.1" {
		t.Fatalf("snapshot: %+v", r)
	}
	if r.Findings[0].ID != "d" || r.Findings[0].Severity != model.SeverityCritical {
		t.Errorf("duplicate identity should keep the most severe, sorted first: %+v", r.Findings)
	}
}

func TestIncompleteDetection(t *testing.T) {
	c := &model.Context{Health: &model.Health{}}
	if got := Incomplete(c, true); len(got) != 0 {
		t.Errorf("complete run flagged: %v", got)
	}
	if got := Incomplete(c, false); len(got) != 1 {
		t.Errorf("missing pg_monitor not flagged: %v", got)
	}
	c.Health.Exactness = model.ExactnessUnavailable
	if got := Incomplete(c, true); len(got) != 1 {
		t.Errorf("unavailable health not flagged: %v", got)
	}
	if got := Incomplete(&model.Context{}, true); len(got) != 1 {
		t.Errorf("nil health not flagged: %v", got)
	}
}
