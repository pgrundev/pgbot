package store

import (
	"database/sql"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

// wait rollup retention: minute granularity for the recent window, then folded
// to hourly out to the far horizon. Reuses the snapshot horizons so the store
// ages uniformly.
const (
	waitKeepMinutesFor = keepAllFor    // 7 days at minute granularity
	waitKeepHoursFor   = keepRollupFor // 90 days at hourly granularity
)

// SaveWaitProfile accumulates one profile's bucket counts into the current
// minute, then ages older minute rows into hourly ones. Best-effort: a nil or
// unavailable profile is a no-op. Raw samples are never stored — only counts.
func (s *Store) SaveWaitProfile(fingerprint string, at time.Time, wp *model.WaitProfile) error {
	if wp == nil || !wp.Available || wp.Samples == 0 {
		return nil
	}
	minute := at.UTC().Truncate(time.Minute).Unix()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	up, err := tx.Prepare(`
		INSERT INTO wait_rollups (target_id, bucket_ts, granularity, wait_type, wait_event, samples)
		VALUES (?, ?, 'minute', ?, ?, ?)
		ON CONFLICT(target_id, bucket_ts, granularity, wait_type, wait_event)
		DO UPDATE SET samples = samples + excluded.samples`)
	if err != nil {
		return err
	}
	defer up.Close()
	for _, b := range wp.Buckets {
		if b.Type == "CPU" || len(b.Events) == 0 {
			if _, err := up.Exec(fingerprint, minute, b.Type, "", b.Count); err != nil {
				return err
			}
			continue
		}
		for _, ev := range b.Events {
			if _, err := up.Exec(fingerprint, minute, b.Type, ev.Event, ev.Count); err != nil {
				return err
			}
		}
	}
	if err := pruneWaits(tx, fingerprint, at.UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

// pruneWaits folds minute buckets older than the minute horizon into hourly
// buckets, deletes those minute rows, then drops hourly rows past the far
// horizon. Runs inside the caller's transaction so fold-then-delete is atomic.
func pruneWaits(tx *sql.Tx, fingerprint string, now time.Time) error {
	minuteCutoff := now.Add(-waitKeepMinutesFor).Unix()
	hourCutoff := now.Add(-waitKeepHoursFor).Unix()

	// 1. Fold aged minute rows up into their hour bucket.
	if _, err := tx.Exec(`
		INSERT INTO wait_rollups (target_id, bucket_ts, granularity, wait_type, wait_event, samples)
		SELECT target_id, (bucket_ts/3600)*3600, 'hour', wait_type, wait_event, sum(samples)
		FROM wait_rollups
		WHERE target_id = ? AND granularity = 'minute' AND bucket_ts < ?
		GROUP BY target_id, (bucket_ts/3600)*3600, wait_type, wait_event
		ON CONFLICT(target_id, bucket_ts, granularity, wait_type, wait_event)
		DO UPDATE SET samples = samples + excluded.samples`,
		fingerprint, minuteCutoff); err != nil {
		return err
	}
	// 2. Remove the folded minute rows.
	if _, err := tx.Exec(`DELETE FROM wait_rollups
		WHERE target_id = ? AND granularity = 'minute' AND bucket_ts < ?`,
		fingerprint, minuteCutoff); err != nil {
		return err
	}
	// 3. Drop hourly rows past the far horizon.
	if _, err := tx.Exec(`DELETE FROM wait_rollups
		WHERE target_id = ? AND granularity = 'hour' AND bucket_ts < ?`,
		fingerprint, hourCutoff); err != nil {
		return err
	}
	return nil
}

// ReadWaitShares returns each wait class's share of all samples recorded for
// the fingerprint between from and to, plus the total sample count — the
// baseline the why-engine compares a live study against. Windows from the
// store are minute/hour rollups of whenever pgbot actually ran; callers
// compare by ratio and must never blend them into live percentages.
func ReadWaitShares(s *Store, fingerprint string, from, to time.Time) (map[string]float64, int, error) {
	rows, err := s.db.Query(`
		SELECT wait_type, sum(samples) FROM wait_rollups
		WHERE target_id = ? AND bucket_ts >= ? AND bucket_ts < ?
		GROUP BY wait_type`, fingerprint, from.UTC().Unix(), to.UTC().Unix())
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	counts := map[string]int64{}
	var total int64
	for rows.Next() {
		var typ string
		var n int64
		if err := rows.Scan(&typ, &n); err != nil {
			return nil, 0, err
		}
		counts[typ] = n
		total += n
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	shares := map[string]float64{}
	if total > 0 {
		for typ, n := range counts {
			shares[typ] = float64(n) / float64(total)
		}
	}
	return shares, int(total), nil
}
