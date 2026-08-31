// Package collect runs the read-only diagnostic SQL and turns it into a
// model.Context. Each domain is a Collector; the runner double-samples counters
// (each sample in its own short transaction) and rate-computes them, and reads
// gauges once. One file per collector; SQL lives in go:embed'd .sql files.
package collect

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/crdbhttp"
	"github.com/pgrundev/pgbot/internal/model"
)

// Kind decides how the runner samples a collector.
type Kind int

const (
	KindGauge   Kind = iota // read once (current state)
	KindCounter             // read twice; the delta over the interval is a rate
)

// Options tune a collection run.
type Options struct {
	Interval      time.Duration    // gap between the two counter samples (default 1s, min 500ms)
	Deadline      time.Duration    // hard cap on total wall time (default 5s + interval)
	RawQueryText  bool             // keep raw pg_stat_activity query text (default: scrub — PII)
	ASHHz         int              // wait-event poll rate in Hz (default 10; 0 disables the sampler)
	ASHWindow     time.Duration    // active-session sampling window (default 5s)
	SchemaOnly    bool             // --profile=schema: run only schema-relevant collectors (D3-1)
	CockroachHTTP *crdbhttp.Client // optional Admin/Prometheus health surfaces
}

// schemaCollectors are the collectors that can produce a schema-scoped finding.
// Under --profile=schema the runner skips the rest, so a PR check doesn't sample
// pg_stat_activity for its ASH window or wait out a counter interval only to throw
// the result away. The profile integration tests are the drift guard: a schema
// finding produced by a collector missing here would fail the acceptance test.
var schemaCollectors = map[string]bool{
	"indexes":   true, // index_invalid, redundant_indexes, fk_unindexed
	"sequences": true, // int4_identity_column
	"tables":    true, // autovacuum_disabled_on_table (reloptions)
}

func (o Options) interval() time.Duration {
	if o.Interval < 500*time.Millisecond {
		return time.Second
	}
	return o.Interval
}

func (o Options) ashHz() int {
	if o.ASHHz < 0 {
		return 0
	}
	return o.ASHHz
}

func (o Options) ashWindow() time.Duration {
	if o.ASHWindow <= 0 {
		return 5 * time.Second
	}
	return o.ASHWindow
}

// sampled holds what one collector produced: A (and B for counters), or an error.
type sampled struct {
	A   any
	B   any
	Err error
	// OwnTxns is how many transactions pgbot itself committed inside the sample
	// window [A, B] — the wait sampler's successful polls. Only the health
	// collector receives it, and subtracts it from the commit delta (PR#1).
	OwnTxns int64
}

// Collector reads one diagnostic domain and writes its section into the Context.
// Availability is checked against the server's Capabilities; an unavailable or
// failed collector marks its section unavailable rather than failing the run.
type Collector interface {
	Name() string
	Kind() Kind
	Available(caps conn.Capabilities) bool
	Sample(ctx context.Context, t *conn.Target, caps conn.Capabilities) (any, error)
	Assemble(c *model.Context, caps conn.Capabilities, s sampled, dt time.Duration, opts Options)
}

// registry is the fixed set of collectors, in report order.
var registry = []Collector{
	healthCollector{},
	activityCollector{},
	cockroachCollector{},
	locksCollector{},
	queriesCollector{},
	tablesCollector{},
	indexesCollector{},
	schemaCollector{},
	walCollector{},
	ioCollector{},
	replicationCollector{},
	settingsCollector{},
	limitsCollector{},
	horizonCollector{},
	sequencesCollector{},
	progressCollector{},
	archiverCollector{},
	checksumsCollector{},
	standbyCollector{},
}

// optionsSampler is implemented only by collectors whose reads need run-level
// configuration. Keeping it optional avoids threading HTTP settings through
// every PostgreSQL collector.
type optionsSampler interface {
	SampleWithOptions(context.Context, *conn.Target, conn.Capabilities, Options) (any, error)
}

func sampleCollector(ctx context.Context, c Collector, t *conn.Target, caps conn.Capabilities, opts Options) (any, error) {
	if configured, ok := c.(optionsSampler); ok {
		return configured.SampleWithOptions(ctx, t, caps, opts)
	}
	return c.Sample(ctx, t, caps)
}

var errUnsupportedOnCockroach = errors.New("collector not yet supported on CockroachDB")

// collectorAvailable is the engine boundary for the incremental CockroachDB
// port. A PostgreSQL collector must be explicitly replaced before it can run on
// CockroachDB; pgwire compatibility alone is not evidence that its catalogs or
// metrics have the same semantics.
func collectorAvailable(c Collector, caps conn.Capabilities) bool {
	if caps.IsCockroachDB() && c.Name() != "activity" && c.Name() != "queries" && c.Name() != "cockroachdb" && c.Name() != "tables" && c.Name() != "indexes" {
		return false
	}
	return c.Available(caps)
}

func nowUTC() time.Time { return time.Now().UTC() }

// unavail builds an unavailable Section, using a (redacted) error as the reason
// when there is one.
func unavail(err error, fallback string) model.Section {
	reason := fallback
	if err != nil {
		reason = conn.RedactConnString(err.Error())
	}
	return model.Section{Exactness: model.ExactnessUnavailable, Reason: reason}
}

// newContext seeds the server/window/findings shell that collectors fill in.
func newContext(caps conn.Capabilities, tB time.Time, dt time.Duration) *model.Context {
	srv := model.ServerInfo{
		Engine:          string(caps.EngineName()),
		VersionNum:      caps.VersionNum,
		VersionText:     caps.VersionText,
		Database:        caps.Database,
		Provider:        string(caps.Provider),
		InRecovery:      caps.Standby(),
		Extensions:      caps.Extensions,
		Capabilities:    caps.Satisfied(),
		HasPgMonitor:    caps.HasPgMonitor,
		HasViewActivity: caps.HasViewActivity,
	}
	window := model.Window{SampleSeconds: round2(dt.Seconds())}
	if !caps.StartedAt.IsZero() {
		started := caps.StartedAt.UTC()
		srv.StartedAt = &started
		srv.UptimeSeconds = int64(tB.Sub(started).Seconds())
		window.PostmasterStartAt = &started
		// Provisional window age (server uptime); the health collector refines it
		// to the stats-reset age when pg_stat_database.stats_reset is present.
		age := int64(tB.Sub(started).Seconds())
		window.WindowAgeSeconds = &age
	}
	return &model.Context{
		SchemaVersion: model.SchemaVersion,
		CollectedAt:   tB,
		Server:        srv,
		Window:        window,
		Findings:      []model.Finding{},
	}
}

// ---- shared query helpers: each runs in its own READ ONLY transaction ----

func queryOne[T any](ctx context.Context, t *conn.Target, sql string, args ...any) (T, error) {
	var out T
	err := t.ReadOnlyTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		out, err = pgx.CollectExactlyOneRow(rows, pgx.RowToStructByNameLax[T])
		return err
	})
	return out, err
}

func queryMany[T any](ctx context.Context, t *conn.Target, sql string, args ...any) ([]T, error) {
	return queryManyLocal[T](ctx, t, nil, sql, args...)
}

// queryManyLocal is queryMany with transaction-local SET statements run first
// (SET LOCAL … — they end with the transaction, so the session's pins stay).
func queryManyLocal[T any](ctx context.Context, t *conn.Target, setLocal []string, sql string, args ...any) ([]T, error) {
	var out []T
	err := t.ReadOnlyTx(ctx, func(tx pgx.Tx) error {
		for _, s := range setLocal {
			if _, err := tx.Exec(ctx, s); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByNameLax[T])
		return err
	})
	return out, err
}

func scalar[T any](ctx context.Context, t *conn.Target, sql string, args ...any) (T, error) {
	var out T
	err := t.ReadOnlyTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&out)
	})
	return out, err
}
