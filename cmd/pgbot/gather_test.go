package main

import (
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/collect"
)

// gatherOptions replaced an inline collect.Options literal. A forwarding helper
// that silently drops a field (interval, ASH rate, window) would still compile
// and still pass a deadline-only check, so pin the whole mapping.
func TestGatherOptionsForwardsEveryCollectionFlag(t *testing.T) {
	f := inspectFlags{
		interval: 750 * time.Millisecond,
		ashHz:    25,
		window:   7 * time.Second,
		timeout:  90 * time.Second,
	}
	want := collect.Options{
		Interval:  750 * time.Millisecond,
		ASHHz:     25,
		ASHWindow: 7 * time.Second,
		Deadline:  90 * time.Second,
	}
	if got := gatherOptions(f); got != want {
		t.Fatalf("gatherOptions(%+v) = %+v; want %+v", f, got, want)
	}
}

// Callers that never set a --timeout (ask, config explain, the MCP tools) pass a
// zero inspectFlags.timeout; that must reach the collector as a zero Deadline so
// its own fallback applies, rather than being replaced by some other default here.
func TestGatherOptionsZeroTimeoutLeavesCollectorFallback(t *testing.T) {
	got := gatherOptions(inspectFlags{interval: time.Second})
	if got.Deadline != 0 {
		t.Fatalf("gatherOptions with no timeout set Deadline = %s; want 0 (collector default)", got.Deadline)
	}
}
