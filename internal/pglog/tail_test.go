package pglog

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSource is an in-memory Source the tests mutate to simulate appends and
// rotation.
type fakeSource struct {
	mu   sync.Mutex
	name string
	data []byte
}

func (f *fakeSource) Stat(context.Context) (TailPos, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return TailPos{Name: f.name, Size: int64(len(f.data))}, nil
}

func (f *fakeSource) ReadAt(_ context.Context, name string, off, length int64) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name != f.name {
		return nil, fmt.Errorf("no such file %q", name)
	}
	if off >= int64(len(f.data)) {
		return nil, nil
	}
	end := off + length
	if end > int64(len(f.data)) {
		end = int64(len(f.data))
	}
	return f.data[off:end], nil
}

func (f *fakeSource) set(name, data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.name, f.data = name, []byte(data)
}

func (f *fakeSource) append(data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = append(f.data, data...)
}

func stderrLine(i int, sev, msg string) string {
	return fmt.Sprintf("2026-08-31 10:00:%02d.000 UTC [%d] %s:  %s\n", i%60, 100+i, sev, msg)
}

// --last N means N entries the caller keeps: when a filter drops entries (own
// noise, level filter), the backwards scan must widen until N survivors exist.
func TestLastNAppliesFilterBeforeCounting(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 40; i++ {
		sev := "LOG"
		if i%10 == 0 {
			sev = "ERROR"
		}
		b.WriteString(stderrLine(i, sev, fmt.Sprintf("event %d", i)))
	}
	src := &fakeSource{}
	src.set("postgresql-1.log", b.String())

	onlyErrors := func(e Entry) bool { return e.Level == LevelError }
	entries, _, err := lastN(context.Background(), src, 3, 128, onlyErrors)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 surviving entries, got %d: %+v", len(entries), entries)
	}
	if entries[0].Message != "event 10" || entries[2].Message != "event 30" {
		t.Errorf("wrong survivors: %+v", entries)
	}
}

func TestLastNReturnsTheTail(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString(stderrLine(i, "LOG", fmt.Sprintf("event %d", i)))
	}
	src := &fakeSource{}
	src.set("postgresql-1.log", b.String())

	entries, pos, err := LastN(context.Background(), src, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 10 {
		t.Fatalf("want 10 entries, got %d", len(entries))
	}
	if got := entries[9].Message; got != "event 49" {
		t.Errorf("newest entry = %q", got)
	}
	if got := entries[0].Message; got != "event 40" {
		t.Errorf("oldest of the tail = %q", got)
	}
	if pos.Offset != int64(b.Len()) {
		t.Errorf("pos.Offset = %d, want EOF %d", pos.Offset, b.Len())
	}
}

// Asking for more entries than the file holds returns them all — including the
// very first one, which must not be dropped as a "partial" line.
func TestLastNWholeFile(t *testing.T) {
	data := stderrLine(0, "LOG", "first") + stderrLine(1, "ERROR", "second")
	src := &fakeSource{}
	src.set("postgresql-1.log", data)

	entries, _, err := LastN(context.Background(), src, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Message != "first" {
		t.Fatalf("want both entries from BOF, got %+v", entries)
	}
}

// A tail window that lands mid-entry must resynchronize on the next entry
// start, and continuation lines at the window edge must not become entries.
func TestLastNResyncsOnEntryBoundary(t *testing.T) {
	data := stderrLine(0, "ERROR", "boom") +
		"2026-08-31 10:00:00.500 UTC [100] DETAIL:  Key (email)=(x@y.z) exists.\n" +
		stderrLine(1, "LOG", "after")
	src := &fakeSource{}
	src.set("postgresql-1.log", data)

	// n=1 with a tiny read chunk forces a window that starts inside the fixture.
	entries, _, err := lastN(context.Background(), src, 1, 64, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Message != "after" {
		t.Fatalf("want the last full entry, got %+v", entries)
	}
}

func TestFollowEmitsAppendsAndSurvivesRotation(t *testing.T) {
	src := &fakeSource{}
	src.set("postgresql-1.log", stderrLine(0, "LOG", "old"))

	_, pos, err := LastN(context.Background(), src, 100, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var mu sync.Mutex
	var got []string
	done := make(chan error, 1)
	go func() {
		done <- Follow(ctx, src, pos, 5*time.Millisecond, func(e Entry) error {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, e.Message)
			if len(got) == 3 {
				cancel()
			}
			return nil
		})
	}()

	src.append(stderrLine(1, "ERROR", "appended one"))
	time.Sleep(30 * time.Millisecond)
	src.append(stderrLine(2, "LOG", "appended two"))
	time.Sleep(30 * time.Millisecond)
	// Rotation: a new file replaces the old; its content must be read from 0.
	src.set("postgresql-2.log", stderrLine(3, "WARNING", "fresh file"))

	if err := <-done; err != nil && ctx.Err() == nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"appended one", "appended two", "fresh file"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("got %v, want %v", got, want)
	}
}
