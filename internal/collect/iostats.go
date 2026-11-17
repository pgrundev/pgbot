package collect

import (
	"context"
	_ "embed"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/rate"
)

//go:embed sql/iostats.sql
var sqlIOStats string

// iostats = pg_stat_io (PG16+) per backend/object/context, double-sampled into
// physical-IO rates and, with track_io_timing on, mean per-op latency. This is
// the evidence that separates "reads miss shared_buffers" (cheap when the kernel
// page cache serves them) from "reads wait on the device" (the latency).
type iostatsCollector struct{}

type ioStatRow struct {
	BackendType string  `db:"backend_type"`
	Object      string  `db:"object"`
	Context     string  `db:"context"`
	Reads       int64   `db:"reads"`
	ReadTime    float64 `db:"read_time"`
	Writes      int64   `db:"writes"`
	WriteTime   float64 `db:"write_time"`
	Fsyncs      int64   `db:"fsyncs"`
	FsyncTime   float64 `db:"fsync_time"`
}

type ioStatsReading struct {
	rows     []ioStatRow
	ioTiming bool
}

func (iostatsCollector) Name() string                          { return "iostats" }
func (iostatsCollector) Kind() Kind                            { return KindCounter }
func (iostatsCollector) Available(caps conn.Capabilities) bool { return caps.HasStatIO() }

func (iostatsCollector) Sample(ctx context.Context, t *conn.Target, _ conn.Capabilities) (any, error) {
	rows, err := queryMany[ioStatRow](ctx, t, sqlIOStats)
	if err != nil {
		return nil, err
	}
	r := ioStatsReading{rows: rows}
	// track_io_timing is per-session-visible but server-set; pgbot never pins it,
	// so current_setting reflects the server. Degrade to "off" if the read fails.
	if v, err := scalar[string](ctx, t, `SELECT current_setting('track_io_timing')`); err == nil {
		r.ioTiming = v == "on"
	}
	return r, nil
}

func (iostatsCollector) Assemble(c *model.Context, caps conn.Capabilities, s sampled, dt time.Duration, _ Options) {
	if !caps.HasStatIO() {
		c.IOStats = &model.IOStats{Section: model.Section{Exactness: model.ExactnessUnavailable, Reason: "pg_stat_io requires PostgreSQL 16+"}}
		return
	}
	a, aok := s.A.(ioStatsReading)
	b, bok := s.B.(ioStatsReading)
	if s.Err != nil || !aok || !bok {
		c.IOStats = &model.IOStats{Section: unavail(s.Err, "pg_stat_io unavailable")}
		return
	}
	secs := dt.Seconds()
	if secs <= 0 {
		c.IOStats = &model.IOStats{Section: model.Section{Exactness: model.ExactnessUnavailable, Reason: "zero sample interval"}}
		return
	}
	// Latency needs timing on at both ends: a mid-window toggle leaves read_time
	// covering only part of the reads and understates the mean.
	timing := a.ioTiming && b.ioTiming
	out := &model.IOStats{Section: model.Section{Exactness: model.ExactnessSampled}, TrackIOTiming: timing}
	key := func(r ioStatRow) string { return r.BackendType + "\x00" + r.Object + "\x00" + r.Context }
	first := map[string]ioStatRow{}
	for _, r := range a.rows {
		first[key(r)] = r
	}
	var allReads, allWrites, allFsyncs int64 // every row, for the rates
	var reads, writes, fsyncs int64          // data rows only, for the latency means
	var readT, writeT, fsyncT float64
	for _, rb := range b.rows {
		ra, ok := first[key(rb)]
		if !ok {
			continue
		}
		dr, okR := rate.Delta(ra.Reads, rb.Reads)
		dw, okW := rate.Delta(ra.Writes, rb.Writes)
		df, okF := rate.Delta(ra.Fsyncs, rb.Fsyncs)
		if !okR || !okW || !okF {
			c.IOStats = &model.IOStats{Section: model.Section{Exactness: model.ExactnessReset, Reason: "pg_stat_io counter reset between samples"}, TrackIOTiming: timing}
			return
		}
		if dr == 0 && dw == 0 && df == 0 {
			continue
		}
		row := model.IOStatRow{
			BackendType: rb.BackendType, Object: rb.Object, Context: rb.Context,
			ReadsPerSec: round2(float64(dr) / secs), WritesPerSec: round2(float64(dw) / secs), FsyncsPerSec: round2(float64(df) / secs),
		}
		// PG18's wal rows are timed by track_wal_io_timing, not track_io_timing:
		// counted but zero-timed by default, so they stay out of every mean.
		data := rb.Object != "wal"
		if timing && data {
			row.ReadLatencyMS = meanMS(rb.ReadTime-ra.ReadTime, dr)
			row.WriteLatencyMS = meanMS(rb.WriteTime-ra.WriteTime, dw)
		}
		out.Rows = append(out.Rows, row)
		allReads += dr
		allWrites += dw
		allFsyncs += df
		if !data {
			continue
		}
		reads += dr
		writes += dw
		fsyncs += df
		readT += rb.ReadTime - ra.ReadTime
		writeT += rb.WriteTime - ra.WriteTime
		fsyncT += rb.FsyncTime - ra.FsyncTime
	}
	r, w, f := round2(float64(allReads)/secs), round2(float64(allWrites)/secs), round2(float64(allFsyncs)/secs)
	out.ReadsPerSec, out.WritesPerSec, out.FsyncsPerSec = &r, &w, &f
	out.ReadsInWindow = reads
	if timing {
		out.ReadLatencyMS = meanMS(readT, reads)
		out.WriteLatencyMS = meanMS(writeT, writes)
		out.FsyncLatencyMS = meanMS(fsyncT, fsyncs)
	}
	c.IOStats = out
}

// meanMS is totalMS/n rounded, or nil when there were no operations (or the
// timing delta went backwards — a reset the counters didn't show).
func meanMS(totalMS float64, n int64) *float64 {
	if n <= 0 || totalMS < 0 {
		return nil
	}
	v := round2(totalMS / float64(n))
	return &v
}
