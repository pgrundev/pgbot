package collect

import (
	"context"
	_ "embed"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
)

//go:embed sql/activity.sql
var sqlActivity string

//go:embed sql/conn_breakdown.sql
var sqlConnBreakdown string

// activity = a point-in-time read of pg_stat_activity.
type activityCollector struct{}

type activityRow struct {
	State         string  `db:"state"`
	WaitEventType string  `db:"wait_event_type"`
	N             int     `db:"n"`
	MaxXactAgeS   float64 `db:"max_xact_age_s"`
	MaxActiveAgeS float64 `db:"max_active_age_s"`
}

func (activityCollector) Name() string                     { return "activity" }
func (activityCollector) Kind() Kind                       { return KindGauge }
func (activityCollector) Available(conn.Capabilities) bool { return true }

type connRow struct {
	AppName  string `db:"app_name"`
	Username string `db:"username"`
	State    string `db:"state"`
	N        int    `db:"n"`
}

type avRow struct {
	Workers int     `db:"workers"`
	MaxAge  float64 `db:"max_age"`
}

type activitySample struct {
	Rows     []activityRow
	Conns    []connRow
	AVWorker avRow
}

func (activityCollector) Sample(ctx context.Context, t *conn.Target, _ conn.Capabilities) (any, error) {
	rows, err := queryMany[activityRow](ctx, t, sqlActivity)
	if err != nil {
		return nil, err
	}
	out := activitySample{Rows: rows}
	out.Conns, _ = queryMany[connRow](ctx, t, sqlConnBreakdown) // best-effort breakdown
	// Autovacuum worker count + longest-running worker (A19), best-effort.
	out.AVWorker, _ = queryOne[avRow](ctx, t, `SELECT count(*) FILTER (WHERE backend_type = 'autovacuum worker')::int AS workers,
		coalesce(max(extract(epoch FROM now() - xact_start)) FILTER (WHERE backend_type = 'autovacuum worker'), 0)::float8 AS max_age
		FROM pg_stat_activity`)
	return out, nil
}

func (activityCollector) Assemble(c *model.Context, _ conn.Capabilities, s sampled, _ time.Duration, _ Options) {
	as, ok := s.A.(activitySample)
	if s.Err != nil || !ok {
		c.Activity = &model.Activity{Section: unavail(s.Err, "pg_stat_activity unavailable")}
		return
	}
	rows := as.Rows
	act := &model.Activity{ByState: map[string]int{}, WaitEvents: map[string]int{}, Section: model.Section{Exactness: model.ExactnessScraped},
		AutovacuumWorkers: as.AVWorker.Workers, AutovacuumMaxAgeSec: round2(as.AVWorker.MaxAge)}
	for _, r := range as.Conns {
		act.Connections = append(act.Connections, model.ConnGroup{
			AppName: r.AppName, User: r.Username, State: r.State, Count: r.N,
		})
	}
	for _, r := range rows {
		act.Total += r.N
		act.ByState[r.State] += r.N
		switch r.State {
		case "active":
			act.Active += r.N
		case "idle":
			act.Idle += r.N
		case "idle in transaction", "idle in transaction (aborted)":
			act.IdleInTransaction += r.N
		}
		if r.WaitEventType != "" {
			act.WaitEvents[r.WaitEventType] += r.N
			// "Waiting" means blocked on something inside the server (Lock, IO,
			// LWLock, …). An idle backend always shows wait_event_type='Client'
			// (ClientRead: waiting for the application to send the next query) —
			// counting it would make every idle connection "waiting".
			if r.WaitEventType != "Client" {
				act.Waiting += r.N
			}
		}
		if r.MaxXactAgeS > act.LongestXactSec {
			act.LongestXactSec = round2(r.MaxXactAgeS)
		}
		if r.MaxActiveAgeS > act.LongestActiveSec {
			act.LongestActiveSec = round2(r.MaxActiveAgeS)
		}
	}
	if len(act.WaitEvents) == 0 {
		act.WaitEvents = nil
	}
	c.Activity = act
}
