package collect

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
)

// WaitSample is one observation of one active backend at one instant. Nil
// WaitEventType means the backend was on CPU (runnable), not waiting.
type WaitSample struct {
	PID           int32   `db:"pid"`
	State         string  `db:"state"`
	WaitEventType *string `db:"wait_event_type"` // NULL ⇒ on CPU
	WaitEvent     *string `db:"wait_event"`
	QueryID       *int64  `db:"query_id"` // pg_stat_activity.query_id, PG14+
	BackendType   string  `db:"backend_type"`
	AppName       string  `db:"app_name"`
}

// ashSQL selects the currently-active backends that are not pgbot's own — the
// pool's collectors run concurrently with the sampler and would otherwise show
// up as CPU/IO samples. query_id is PG14+; on older servers we select a NULL
// literal so the column reference never errors.
func ashSQL(caps conn.Capabilities) string {
	qid := "NULL::bigint"
	if caps.VersionNum >= 140000 {
		qid = "query_id"
	}
	return `SELECT pid, state, wait_event_type, wait_event, ` + qid + ` AS query_id,
	               backend_type, coalesce(application_name, '') AS app_name
	        FROM pg_stat_activity
	        WHERE state = 'active' AND application_name IS DISTINCT FROM 'pgbot'`
}

// ashResult is what one sampling window produced. attempts/failures let the
// caller tell an IDLE database (polls succeeded, found nothing active) from a
// BROKEN sampler (every poll errored) — the former is a real 0-sample profile,
// the latter must be marked unavailable rather than shown as "idle".
type ashResult struct {
	samples  []WaitSample
	span     time.Duration
	attempts int
	failures int
}

// sampleWaits polls active backends at hz for the window, each poll its own
// short read-only transaction. A poll that errors or times out is dropped and
// sampling continues — the sampler must never fail the run or queue behind a
// lock storm.
func sampleWaits(ctx context.Context, t *conn.Target, caps conn.Capabilities, hz int, window time.Duration) ashResult {
	if hz <= 0 || window <= 0 {
		return ashResult{}
	}
	sql := ashSQL(caps)
	interval := time.Second / time.Duration(hz)
	start := time.Now()
	deadline := start.Add(window)

	res := ashResult{}
	poll := func() {
		// Each poll gets its own budget so a stalled read is abandoned, not queued.
		pctx, cancel := context.WithTimeout(ctx, interval)
		defer cancel()
		res.attempts++
		rows, err := queryMany[WaitSample](pctx, t, sql)
		if err != nil {
			res.failures++ // drop this poll, keep going
			return
		}
		res.samples = append(res.samples, rows...)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	poll() // first sample immediately
	for {
		select {
		case <-ctx.Done():
			res.span = time.Since(start)
			return res
		case now := <-ticker.C:
			if now.After(deadline) {
				res.span = time.Since(start)
				return res
			}
			poll()
		}
	}
}

// profileFrom turns a sampling result into a profile, distinguishing a BROKEN
// sampler (every poll errored → unavailable) from an IDLE database (polls
// succeeded, nothing active → a real 0-sample profile). Either way the run
// completes — the sampler never fails it.
func profileFrom(ash ashResult, texts map[int64]string) *model.WaitProfile {
	if ash.attempts > 0 && ash.failures == ash.attempts {
		return &model.WaitProfile{Available: false,
			Reason: fmt.Sprintf("wait sampler failed: all %d polls errored", ash.attempts)}
	}
	return buildWaitProfile(ash.samples, ash.span, texts)
}

// buildWaitProfile rolls raw samples into shares by wait_event_type (with a
// synthetic CPU bucket for on-CPU samples), each broken down by wait_event, and
// attributes time per query_id where the server supplied one. texts maps a
// query_id to its scrubbed normalized text (best-effort, from the queries
// collector). The profile is a distribution over a SAMPLE — it always carries
// its sample count so a thin sample can be flagged rather than dressed up.
func buildWaitProfile(samples []WaitSample, window time.Duration, texts map[int64]string) *model.WaitProfile {
	wp := &model.WaitProfile{Available: true, Samples: len(samples), WindowSeconds: round2(window.Seconds())}
	if len(samples) == 0 {
		return wp
	}
	total := len(samples)

	byType := map[string]int{}
	byTypeEvent := map[string]map[string]int{}
	byQuery := map[int64]*queryAgg{}

	for _, s := range samples {
		typ := "CPU"
		if s.WaitEventType != nil && *s.WaitEventType != "" {
			typ = *s.WaitEventType
		}
		ev := ""
		if s.WaitEvent != nil {
			ev = *s.WaitEvent
		}
		byType[typ]++
		if typ != "CPU" && ev != "" {
			if byTypeEvent[typ] == nil {
				byTypeEvent[typ] = map[string]int{}
			}
			byTypeEvent[typ][ev]++
		}
		if s.QueryID != nil && *s.QueryID != 0 {
			q := byQuery[*s.QueryID]
			if q == nil {
				q = &queryAgg{typeCount: map[string]int{}, eventCount: map[string]int{}}
				byQuery[*s.QueryID] = q
			}
			q.count++
			q.typeCount[typ]++
			key := typ
			if ev != "" {
				key = typ + ":" + ev
			}
			q.eventCount[key]++
		}
	}

	for typ, n := range byType {
		b := model.WaitBucket{Type: typ, Count: n, Share: float64(n) / float64(total)}
		for ev, en := range byTypeEvent[typ] {
			b.Events = append(b.Events, model.WaitEvent{Event: ev, Count: en, Share: float64(en) / float64(total)})
		}
		sort.Slice(b.Events, func(i, j int) bool { return b.Events[i].Count > b.Events[j].Count })
		wp.Buckets = append(wp.Buckets, b)
	}
	sort.Slice(wp.Buckets, func(i, j int) bool {
		if wp.Buckets[i].Count != wp.Buckets[j].Count {
			return wp.Buckets[i].Count > wp.Buckets[j].Count
		}
		return wp.Buckets[i].Type < wp.Buckets[j].Type
	})

	for qid, q := range byQuery {
		topType, topKey := topKey(q.typeCount), topKey(q.eventCount)
		qw := model.QueryWaits{
			QueryID:    qid,
			SampleText: texts[qid],
			Count:      q.count,
			Share:      float64(q.count) / float64(total),
			LockShare:  float64(q.typeCount["Lock"]) / float64(q.count),
			IOShare:    float64(q.typeCount["IO"]) / float64(q.count),
			TopType:    topType,
			TopEvent:   topKey,
		}
		wp.ByQuery = append(wp.ByQuery, qw)
	}
	sort.Slice(wp.ByQuery, func(i, j int) bool {
		if wp.ByQuery[i].Count != wp.ByQuery[j].Count {
			return wp.ByQuery[i].Count > wp.ByQuery[j].Count
		}
		return wp.ByQuery[i].QueryID < wp.ByQuery[j].QueryID
	})
	return wp
}

type queryAgg struct {
	count      int
	typeCount  map[string]int
	eventCount map[string]int
}

// topKey returns the key with the largest count (ties broken lexicographically
// for determinism), or "" for an empty map.
func topKey(m map[string]int) string {
	best, bestN := "", -1
	for k, n := range m {
		if n > bestN || (n == bestN && k < best) {
			best, bestN = k, n
		}
	}
	return best
}
