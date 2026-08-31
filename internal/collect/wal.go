package collect

import (
	"context"
	_ "embed"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/rate"
)

//go:embed sql/wal.sql
var sqlWAL string

// wal = pg_stat_wal (PG14+), double-sampled for byte/record rates.
type walCollector struct{}

type walSample struct {
	WalRecords     int64 `db:"wal_records"`
	WalBytes       int64 `db:"wal_bytes"`
	WalBuffersFull int64 `db:"wal_buffers_full"`
}

type walDirRow struct {
	Files int64 `db:"files"`
	Bytes int64 `db:"bytes"`
}

// walReading bundles the pg_stat_wal counters with the pg_wal directory size
// (A14). The directory size pairs with replication_slot_inactive: "the slot is
// retaining 8 GB AND pg_wal is now 34 GB" is a far stronger signal than either.
type walReading struct {
	stat     walSample
	dirBytes int64
	dirFiles int64
	dirRead  bool
}

func (walCollector) Name() string                          { return "wal" }
func (walCollector) Kind() Kind                            { return KindCounter }
func (walCollector) Available(caps conn.Capabilities) bool { return caps.HasStatWAL() }

func (walCollector) Sample(ctx context.Context, t *conn.Target, _ conn.Capabilities) (any, error) {
	stat, err := queryOne[walSample](ctx, t, sqlWAL)
	if err != nil {
		return nil, err
	}
	r := walReading{stat: stat}
	// pg_ls_waldir() is executable by pg_monitor; degrade to no dir size if denied.
	if d, err := queryOne[walDirRow](ctx, t,
		`SELECT count(*)::bigint AS files, coalesce(sum(size),0)::bigint AS bytes FROM pg_ls_waldir()`); err == nil {
		r.dirBytes, r.dirFiles, r.dirRead = d.Bytes, d.Files, true
	}
	return r, nil
}

func (walCollector) Assemble(c *model.Context, caps conn.Capabilities, s sampled, dt time.Duration, _ Options) {
	if caps.IsCockroachDB() {
		c.WAL = &model.WAL{Section: unavail(errUnsupportedOnCockroach, "")}
		return
	}
	if !caps.HasStatWAL() {
		c.WAL = &model.WAL{Section: model.Section{Exactness: model.ExactnessUnavailable, Reason: "pg_stat_wal requires PostgreSQL 14+"}}
		return
	}
	a, aok := s.A.(walReading)
	b, bok := s.B.(walReading)
	if s.Err != nil || !aok || !bok {
		c.WAL = &model.WAL{Section: unavail(s.Err, "pg_stat_wal unavailable")}
		return
	}
	w := &model.WAL{Section: model.Section{Exactness: model.ExactnessSampled}, BuffersFull: b.stat.WalBuffersFull}
	if bp, ok := rate.PerSecond(a.stat.WalBytes, b.stat.WalBytes, dt); ok {
		w.BytesPerSec = round2p(bp)
	} else {
		w.Section = model.Section{Exactness: model.ExactnessReset, Reason: "wal counter reset between samples"}
	}
	if rp, ok := rate.PerSecond(a.stat.WalRecords, b.stat.WalRecords, dt); ok {
		w.RecordsPerSec = round2p(rp)
	}
	if b.dirRead {
		bytes := b.dirBytes
		w.DirBytes = &bytes
		w.DirFiles = b.dirFiles
	}
	c.WAL = w
}
