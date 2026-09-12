package collect

import (
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
)

func TestIOStats_assembleRatesAndLatency(t *testing.T) {
	caps := conn.Capabilities{VersionNum: 180000}
	row := func(reads int64, readT float64, writes int64) ioStatRow {
		return ioStatRow{BackendType: "client backend", Object: "relation", Context: "normal", Reads: reads, ReadTime: readT, Writes: writes}
	}
	idle := ioStatRow{BackendType: "checkpointer", Object: "relation", Context: "normal"}
	a := ioStatsReading{rows: []ioStatRow{row(1000, 500, 10), idle}, ioTiming: true}
	b := ioStatsReading{rows: []ioStatRow{row(3000, 1500, 30), idle}, ioTiming: true}

	c := &model.Context{}
	iostatsCollector{}.Assemble(c, caps, sampled{A: a, B: b}, 10*time.Second, Options{})
	io := c.IOStats
	if io == nil || io.Exactness != model.ExactnessSampled {
		t.Fatalf("want sampled section, got %+v", io)
	}
	if io.ReadsPerSec == nil || *io.ReadsPerSec != 200 || io.ReadsInWindow != 2000 {
		t.Errorf("reads: want 200/s over 2000, got %+v", io)
	}
	if io.ReadLatencyMS == nil || *io.ReadLatencyMS != 0.5 {
		t.Errorf("latency: want 0.5 ms (1000 ms / 2000 reads), got %v", io.ReadLatencyMS)
	}
	if len(io.Rows) != 1 {
		t.Errorf("idle rows must be dropped, got %d rows", len(io.Rows))
	}

	// track_io_timing off → rates but no latency.
	a.ioTiming, b.ioTiming = false, false
	c = &model.Context{}
	iostatsCollector{}.Assemble(c, caps, sampled{A: a, B: b}, 10*time.Second, Options{})
	if c.IOStats.TrackIOTiming || c.IOStats.ReadLatencyMS != nil || c.IOStats.ReadsPerSec == nil {
		t.Errorf("without track_io_timing: rates yes, latency nil, got %+v", c.IOStats)
	}

	// Counter reset → exactness reset, no rates.
	c = &model.Context{}
	iostatsCollector{}.Assemble(c, caps, sampled{A: b, B: a}, 10*time.Second, Options{})
	if c.IOStats.Exactness != model.ExactnessReset || c.IOStats.ReadsPerSec != nil {
		t.Errorf("reset must be flagged, got %+v", c.IOStats)
	}

	// PG15 → unavailable.
	c = &model.Context{}
	iostatsCollector{}.Assemble(c, conn.Capabilities{VersionNum: 150000}, sampled{}, time.Second, Options{})
	if c.IOStats.Exactness != model.ExactnessUnavailable {
		t.Errorf("PG15 must be unavailable, got %+v", c.IOStats)
	}
}
