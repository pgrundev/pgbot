package pglog

import (
	"context"
	"time"
)

// Source is where log bytes come from — pg_read_file over SQL, or (later) a
// local file. Reads are addressed by file name so rotation is detectable.
type Source interface {
	// Stat identifies the current logfile and its size.
	Stat(ctx context.Context) (TailPos, error)
	// ReadAt returns up to length bytes of the named file from off. A short
	// (or empty) result is not an error.
	ReadAt(ctx context.Context, name string, off, length int64) ([]byte, error)
}

// TailPos is a position in the log stream: which file, how big it was at the
// last look, and how far we've consumed it.
type TailPos struct {
	Name   string
	Size   int64
	Offset int64
}

const (
	lastNChunk   = 256 * 1024  // backwards-scan step for LastN
	maxReadChunk = 1024 * 1024 // per-poll read cap in Follow
)

// LastN returns the newest n complete entries that pass keep (nil keeps all)
// and the EOF position a Follow can continue from. Counting after the filter
// is the point: "last 100" means 100 entries the caller will actually see,
// however much filtered-out noise sits between them.
func LastN(ctx context.Context, src Source, n int, keep func(Entry) bool) ([]Entry, TailPos, error) {
	return lastN(ctx, src, n, lastNChunk, keep)
}

func lastN(ctx context.Context, src Source, n int, chunk int64, keep func(Entry) bool) ([]Entry, TailPos, error) {
	pos, err := src.Stat(ctx)
	if err != nil {
		return nil, TailPos{}, err
	}
	format := FormatForFile(pos.Name)

	// Read backwards in growing windows until the window parses to ≥ n entries
	// or reaches the beginning of the file.
	window := chunk
	for {
		start := pos.Size - window
		if start < 0 {
			start = 0
		}
		data, err := src.ReadAt(ctx, pos.Name, start, pos.Size-start)
		if err != nil {
			return nil, TailPos{}, err
		}
		p := NewParser(format)
		if start > 0 {
			// The window almost certainly starts mid-entry: resynchronize on
			// the next entry-start line before parsing.
			data = p.SkipPartial(data)
		}
		entries := p.Parse(data)
		entries = append(entries, p.Flush()...)
		if keep != nil {
			filtered := entries[:0]
			for _, e := range entries {
				if keep(e) {
					filtered = append(filtered, e)
				}
			}
			entries = filtered
		}
		if len(entries) >= n || start == 0 {
			if len(entries) > n {
				entries = entries[len(entries)-n:]
			}
			pos.Offset = pos.Size
			return entries, pos, nil
		}
		window *= 4
	}
}

// Follow polls the source and emits each new complete entry, surviving log
// rotation (a new current file is read from its beginning). It returns when
// ctx is done or emit returns an error.
//
// A pending entry (one that may still grow continuation lines) is emitted on
// the first poll that finds no new bytes: PostgreSQL writes a message and its
// DETAIL in one write, so a quiet file means the entry is complete.
func Follow(ctx context.Context, src Source, pos TailPos, interval time.Duration, emit func(Entry) error) error {
	parser := NewParser(FormatForFile(pos.Name))
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}

		cur, err := src.Stat(ctx)
		if err != nil {
			// Transient (rotation race, connection blip): keep polling.
			continue
		}
		if cur.Name != pos.Name || cur.Size < pos.Offset {
			// Rotated (new file) or truncated: drain the old parser, restart
			// at the beginning of what's current.
			for _, e := range parser.Flush() {
				if err := emit(e); err != nil {
					return err
				}
			}
			parser = NewParser(FormatForFile(cur.Name))
			pos = TailPos{Name: cur.Name, Size: cur.Size}
		}

		if cur.Size == pos.Offset {
			for _, e := range parser.Flush() {
				if err := emit(e); err != nil {
					return err
				}
			}
			continue
		}

		length := cur.Size - pos.Offset
		if length > maxReadChunk {
			length = maxReadChunk
		}
		data, err := src.ReadAt(ctx, pos.Name, pos.Offset, length)
		if err != nil {
			continue
		}
		pos.Offset += int64(len(data))
		for _, e := range parser.Parse(data) {
			if err := emit(e); err != nil {
				return err
			}
		}
	}
}
