package monitor

import (
	"fmt"
	"strings"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

// Changes is what moved between the previous successful run and this one.
type Changes struct {
	New         []FindingState // not present before (warn or critical)
	Escalated   []FindingState // present before at a lower severity (warn → critical)
	Deescalated []FindingState // present before at a higher severity (critical → warn)
	Resolved    []FindingState // present before, gone now (carries the OLD severity)
	Unchanged   []FindingState
}

// Compare diffs two finding sets by identity (ID + object), never by title.
func Compare(prev, cur []FindingState) Changes {
	before := make(map[string]FindingState, len(prev))
	for _, f := range prev {
		before[f.Key()] = f
	}
	var ch Changes
	for _, f := range cur {
		old, ok := before[f.Key()]
		switch {
		case !ok:
			ch.New = append(ch.New, f)
		case rank(f.Severity) > rank(old.Severity):
			ch.Escalated = append(ch.Escalated, f)
		case rank(f.Severity) < rank(old.Severity):
			ch.Deescalated = append(ch.Deescalated, f)
		default:
			ch.Unchanged = append(ch.Unchanged, f)
		}
		delete(before, f.Key())
	}
	for _, f := range prev {
		if _, gone := before[f.Key()]; gone {
			ch.Resolved = append(ch.Resolved, f)
		}
	}
	for _, s := range [][]FindingState{ch.New, ch.Escalated, ch.Deescalated, ch.Resolved, ch.Unchanged} {
		sortFindings(s)
	}
	return ch
}

// worsened is New+Escalated filtered to a minimum severity.
func (ch Changes) worsened(minSev string) []FindingState {
	var out []FindingState
	for _, f := range append(append([]FindingState{}, ch.Escalated...), ch.New...) {
		if rank(f.Severity) >= rank(minSev) {
			out = append(out, f)
		}
	}
	sortFindings(out)
	return out
}

// improved reports whether anything at or above minSev went away (resolved,
// or a critical stepped down to a warning).
func (ch Changes) improved(minSev string) bool {
	for _, f := range ch.Resolved {
		if rank(f.Severity) >= rank(minSev) {
			return true
		}
	}
	// Only warn/critical are tracked, so a de-escalation is always critical →
	// warning: an improvement under either threshold.
	return len(ch.Deescalated) > 0
}

// Notification is one Notification Center banner.
type Notification struct {
	Title string
	Body  string
	Kind  string // ok | warn | crit | fail | info — for the terminal echo
}

// Outcome is the result of one check, success or failure.
type Outcome struct {
	At     time.Time
	Run    *RunRecord // nil on failure
	Err    error      // non-nil on failure (already redacted by the caller)
	Manual bool       // `monitor now` rather than launchd
}

// Decision is what to tell the user, and the new state to persist.
type Decision struct {
	Notify  *Notification // nil = stay quiet
	Changes Changes
	Summary []string // terminal lines describing what changed
	State   *State
}

// dedupeWindow suppresses a byte-identical banner posted this recently (e.g.
// `monitor now` immediately after a scheduled run under the "every" policy).
const dedupeWindow = 10 * time.Minute

// Decide applies the notification policy to an outcome. It is pure: it takes
// the previous state and returns the next one, so every transition is
// unit-testable without a database, launchd, or Notification Center.
func Decide(policy Policy, dbName string, prev *State, out Outcome) Decision {
	next := *prev
	next.Activity = append([]Activity(nil), prev.Activity...)
	next.LastAttempt = out.At
	name := DisplayName(dbName)
	d := Decision{State: &next}

	if out.Err != nil {
		errText := Redact(out.Err.Error())
		f := ConnFailure{Since: out.At, Count: 1, LastErr: errText}
		if prev.Failure != nil {
			f = *prev.Failure
			f.Count++
			f.LastErr = errText
		}
		first := prev.Failure == nil || !prev.Failure.Notified
		if first || policy == PolicyEvery {
			d.Notify = &Notification{
				Kind:  "fail",
				Title: "🔴 PgBot · Connection failed",
				Body:  fmt.Sprintf("Cannot reach %s. The scheduled health check could not complete.", name),
			}
			if out.Manual {
				d.Notify.Body = fmt.Sprintf("Cannot reach %s. The health check could not complete.", name)
			}
			f.Notified = true
		}
		next.Failure = &f
		d.Summary = append(d.Summary, "Check failed: "+errText)
		if f.Count > 1 {
			d.Summary = append(d.Summary, fmt.Sprintf("%d consecutive failed checks since %s.", f.Count, HumanWhen(f.Since, out.At)))
		}
		next.log(out.At, "fail", "Connection failed")
		// LastSuccess is deliberately untouched: a failure never overwrites the
		// last known health state.
		return d.dedupe(prev, out.At)
	}

	cur := out.Run
	restored := prev.Failure != nil
	restoredNotified := restored && prev.Failure.Notified
	next.Failure = nil
	next.LastSuccess = cur

	var base []FindingState
	first := prev.LastSuccess == nil || prev.LastSuccess.Fingerprint != cur.Fingerprint
	if !first {
		base = prev.LastSuccess.Findings
	}
	ch := Compare(base, cur.Findings)
	d.Changes = ch
	crit, warn := cur.Counts()
	prevScore := -1
	if !first {
		prevScore = prev.LastSuccess.Score
	}

	// Terminal summary + activity log: always, whatever the policy.
	d.Summary = summaryLines(ch, first, restored)
	switch {
	case crit > 0:
		next.log(out.At, "crit", plural(crit, "critical issue", "critical issues")+" open")
	case warn > 0:
		next.log(out.At, "warn", plural(warn, "warning", "warnings")+" open")
	case len(cur.Incomplete) > 0:
		next.log(out.At, "info", "Check completed with partial data")
	default:
		next.log(out.At, "ok", "Database healthy")
	}
	if restored {
		next.log(out.At, "ok", "Connection restored")
	}

	minSev := model.SeverityWarn
	if policy == PolicyCritical {
		minSev = model.SeverityCritical
	}
	worse := ch.worsened(minSev)

	switch {
	case first:
		d.Notify = initialNotification(name, cur)
	case len(worse) > 0:
		d.Notify = problemNotification(name, cur, worse)
	case restored && restoredNotified:
		d.Notify = restoredNotification(name, cur, ch)
	case ch.improved(minSev) && policy != PolicyEvery:
		d.Notify = recoveryNotification(name, cur, prevScore, ch)
	case policy == PolicyEvery:
		if ch.improved(model.SeverityWarn) {
			d.Notify = recoveryNotification(name, cur, prevScore, ch)
		} else {
			d.Notify = summaryNotification(name, cur)
		}
	}
	return d.dedupe(prev, out.At)
}

func (d Decision) dedupe(prev *State, now time.Time) Decision {
	if d.Notify == nil {
		return d
	}
	d.Notify.Title = clip(Redact(d.Notify.Title), maxTitle)
	d.Notify.Body = clip(Redact(d.Notify.Body), maxBody)
	if ls := prev.LastSent; ls != nil && ls.Title == d.Notify.Title && ls.Body == d.Notify.Body && now.Sub(ls.At) < dedupeWindow {
		d.Summary = append(d.Summary, "Same notification was sent "+now.Sub(ls.At).Round(time.Second).String()+" ago — not repeating it.")
		d.Notify = nil
		return d
	}
	d.State.LastSent = &Sent{At: now, Title: d.Notify.Title, Body: d.Notify.Body}
	return d
}

func summaryLines(ch Changes, first, restored bool) []string {
	var out []string
	if restored {
		out = append(out, "Connection restored.")
	}
	if first {
		out = append(out, "First check — this is the starting point for change detection.")
		return out
	}
	if n := len(ch.New); n > 0 {
		out = append(out, plural(n, "new issue", "new issues")+".")
	}
	if n := len(ch.Escalated); n > 0 {
		out = append(out, plural(n, "issue became more severe", "issues became more severe")+".")
	}
	if n := len(ch.Resolved); n > 0 {
		out = append(out, plural(n, "previous issue resolved", "previous issues resolved")+".")
	}
	if n := len(ch.Deescalated); n > 0 {
		out = append(out, plural(n, "issue became less severe", "issues became less severe")+".")
	}
	if len(ch.New)+len(ch.Escalated) == 0 {
		out = append(out, "No new warnings.")
	}
	return out
}

func initialNotification(name string, r *RunRecord) *Notification {
	crit, warn := r.Counts()
	switch {
	case crit > 0:
		return problemNotification(name, r, r.Findings)
	case warn > 0:
		return problemNotification(name, r, r.Findings)
	case len(r.Incomplete) > 0:
		return incompleteNotification(name, r)
	}
	return &Notification{
		Kind:  "ok",
		Title: fmt.Sprintf("🐘 PgBot · %s is healthy", name),
		Body:  fmt.Sprintf("Health score: %d/100. All critical checks passed. No issues detected.", r.Score),
	}
}

func problemNotification(name string, r *RunRecord, highlight []FindingState) *Notification {
	crit, _ := r.Counts()
	n := &Notification{Kind: "warn", Title: "⚠️ PgBot · " + name + " needs attention"}
	if crit > 0 && hasSeverity(highlight, model.SeverityCritical) {
		n.Kind, n.Title = "crit", "🚨 PgBot · Critical issue on "+name
	}
	n.Body = fmt.Sprintf("Health %d/100. %s", r.Score, findingList(highlight))
	if n.Kind == "crit" {
		n.Body += " Review immediately."
	}
	return n
}

func recoveryNotification(name string, r *RunRecord, prevScore int, ch Changes) *Notification {
	crit, warn := r.Counts()
	n := &Notification{Kind: "ok"}
	if crit+warn == 0 && len(r.Incomplete) == 0 {
		n.Title = "✅ PgBot · " + name + " recovered"
		n.Body = "Previously reported issues are resolved."
	} else {
		n.Title = "✅ PgBot · " + name + " is improving"
		n.Body = plural(len(ch.Resolved)+len(ch.Deescalated), "issue", "issues") + " improved; " +
			plural(crit+warn, "issue remains", "issues remain") + "."
	}
	if prevScore >= 0 && r.Score > prevScore {
		n.Body += fmt.Sprintf(" Health score improved from %d to %d.", prevScore, r.Score)
	} else {
		n.Body += fmt.Sprintf(" Health score: %d/100.", r.Score)
	}
	return n
}

func restoredNotification(name string, r *RunRecord, ch Changes) *Notification {
	crit, warn := r.Counts()
	n := &Notification{Kind: "ok", Title: "✅ PgBot · Connection restored"}
	n.Body = fmt.Sprintf("%s is reachable again. Health %d/100", name, r.Score)
	switch {
	case crit+warn > 0:
		n.Body += ", " + plural(crit+warn, "open issue", "open issues") + "."
	case len(r.Incomplete) > 0:
		n.Body += " (partial data)."
	default:
		n.Body += ", no open issues."
	}
	return n
}

func summaryNotification(name string, r *RunRecord) *Notification {
	crit, warn := r.Counts()
	switch {
	case crit > 0:
		n := problemNotification(name, r, r.Findings)
		n.Body = strings.TrimSuffix(n.Body, " Review immediately.") + " (unchanged)"
		return n
	case warn > 0:
		n := problemNotification(name, r, r.Findings)
		n.Body += " (unchanged)"
		return n
	case len(r.Incomplete) > 0:
		return incompleteNotification(name, r)
	}
	return &Notification{
		Kind:  "ok",
		Title: fmt.Sprintf("🐘 PgBot · %s is healthy", name),
		Body:  fmt.Sprintf("Health score: %d/100. All critical checks passed. No new issues detected.", r.Score),
	}
}

// incompleteNotification is used instead of a "healthy" banner when the data
// can't support that claim.
func incompleteNotification(name string, r *RunRecord) *Notification {
	return &Notification{
		Kind:  "info",
		Title: "🐘 PgBot · " + name + " checked (partial data)",
		Body:  fmt.Sprintf("No warnings found, but %s. Score %d/100 is not a full verdict.", r.Incomplete[0], r.Score),
	}
}

// findingList renders at most two titles and summarizes the rest.
func findingList(fs []FindingState) string {
	var parts []string
	for i, f := range fs {
		if i == 2 {
			break
		}
		parts = append(parts, sentence(f.Title))
	}
	s := strings.Join(parts, " ")
	if extra := len(fs) - 2; extra > 0 {
		s += fmt.Sprintf(" +%d more.", extra)
	}
	return s
}

func hasSeverity(fs []FindingState, sev string) bool {
	for _, f := range fs {
		if f.Severity == sev {
			return true
		}
	}
	return false
}

func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	if !strings.ContainsAny(s[len(s)-1:], ".!?") {
		s += "."
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// DisplayName capitalizes a database name for banner titles: "production" →
// "Production". Empty becomes "Database".
func DisplayName(db string) string {
	if db == "" {
		return "Database"
	}
	r := []rune(db)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] -= 'a' - 'A'
	}
	return string(r)
}
