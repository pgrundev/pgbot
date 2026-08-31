package collect

import (
	"context"
	_ "embed"
	"hash/fnv"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
)

//go:embed sql/cockroach_queries.sql
var sqlCockroachQueries string

//go:embed sql/cockroach_query_activity.sql
var sqlCockroachQueryActivity string

const (
	cockroachStatsSourceActivityCache = "activity_cache"
	cockroachStatsSourcePublic        = "public_statistics"
)

type cockroachQueryRow struct {
	Fingerprint  string   `db:"fingerprint"`
	AppName      string   `db:"app_name"`
	Query        string   `db:"query"`
	Calls        int64    `db:"calls"`
	TotalMS      float64  `db:"total_ms"`
	MeanMS       float64  `db:"mean_ms"`
	P99MS        *float64 `db:"p99_ms"`
	RowsRead     int64    `db:"rows_read"`
	RowsWritten  int64    `db:"rows_written"`
	BytesRead    int64    `db:"bytes_read"`
	ContentionMS float64  `db:"contention_ms"`
	MaxRetries   int64    `db:"max_retries"`
	TotalExecAll float64  `db:"total_exec_all"`
}

type cockroachQueriesSample struct {
	Rows        []cockroachQueryRow
	Source      string
	WindowHours int
	Bounded     bool
}

func sampleCockroachQueries(ctx context.Context, t *conn.Target, caps conn.Capabilities) (any, error) {
	s := cockroachQueriesSample{Source: cockroachStatsSourcePublic, WindowHours: 1}
	sql := sqlCockroachQueries
	if caps.HasCRDBStmtActivity {
		s.Source = cockroachStatsSourceActivityCache
		s.WindowHours = 24
		s.Bounded = true
		sql = sqlCockroachQueryActivity
	}
	rows, err := queryMany[cockroachQueryRow](ctx, t, sql)
	s.Rows = rows
	return s, err
}

func assembleCockroachQueries(c *model.Context, caps conn.Capabilities, s sampled) {
	if !caps.HasViewActivity {
		c.Queries = &model.Queries{Enabled: false, Section: unavail(nil, "role lacks VIEWACTIVITY — persisted statement statistics are not visible")}
		return
	}
	if !caps.HasCRDBStmtStats {
		c.Queries = &model.Queries{Enabled: false, Section: unavail(nil, "CockroachDB persisted statement statistics are not available on this version")}
		return
	}
	sm, ok := s.A.(cockroachQueriesSample)
	if s.Err != nil || !ok {
		c.Queries = &model.Queries{Enabled: false, Section: unavail(s.Err, "CockroachDB statement statistics read failed")}
		return
	}
	rows := sm.Rows
	q := &model.Queries{
		Enabled: true, Section: model.Section{Exactness: model.ExactnessCumulative},
		StatsSource: sm.Source, WindowHours: sm.WindowHours, Bounded: sm.Bounded,
	}
	if len(rows) > 0 {
		q.TotalExecMS = round2(rows[0].TotalExecAll)
	}
	for _, r := range rows {
		q.Top = append(q.Top, model.QueryStat{
			QueryID: cockroachQueryID(r.Fingerprint, r.AppName), Fingerprint: r.Fingerprint,
			AppName: r.AppName, Query: conn.ScrubRedactableText(r.Query), Calls: r.Calls,
			TotalMS: round2(r.TotalMS), MeanMS: round4(r.MeanMS), P99MS: round2p(r.P99MS),
			RowsRead: r.RowsRead, RowsWritten: r.RowsWritten, BytesRead: r.BytesRead,
			ContentionMS: round2(r.ContentionMS), MaxRetries: r.MaxRetries,
		})
	}
	c.Queries = q
}

func cockroachQueryID(fingerprint, app string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(fingerprint))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(app))
	id := int64(h.Sum64() & uint64(^uint64(0)>>1))
	if id == 0 {
		return 1
	}
	return id
}
