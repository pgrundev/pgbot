package store

import (
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

// Retention policy: keep every snapshot for 7 days, then thin to one per hour
// out to 90 days, then drop. A hard 100 MB cap evicts oldest-first as a
// backstop. Users can see and delete what's stored with `pgbot baselines`.
const (
	keepAllFor    = 7 * 24 * time.Hour
	keepRollupFor = 90 * 24 * time.Hour
)

// maxBytes is a var (not const) only so the eviction test can lower the cap and
// drive enforceSizeCap without writing 100 MB of snapshots.
var maxBytes int64 = 100 << 20

// prune enforces the policy for one fingerprint (called after each Save).
func (s *Store) prune(fingerprint string) error {
	now := time.Now().UTC()

	// 1. Drop everything older than the rollup horizon.
	if _, err := s.db.Exec(
		`DELETE FROM snapshots WHERE fingerprint = ? AND collected_at < ?`,
		fingerprint, now.Add(-keepRollupFor).Unix()); err != nil {
		return err
	}

	// 2. Thin 7d..90d to one row per hour bucket (keep the newest in each).
	rollupCutoff := now.Add(-keepAllFor).Unix()
	if _, err := s.db.Exec(`
		DELETE FROM snapshots
		WHERE fingerprint = ? AND collected_at < ?
		  AND id NOT IN (
			SELECT max(id) FROM snapshots
			WHERE fingerprint = ? AND collected_at < ?
			GROUP BY collected_at / 3600
		  )`, fingerprint, rollupCutoff, fingerprint, rollupCutoff); err != nil {
		return err
	}

	// 3. Size backstop: if the DB file exceeds the cap, evict oldest globally.
	return s.enforceSizeCap()
}

func (s *Store) enforceSizeCap() error {
	size, err := s.fileSize()
	if err != nil {
		return err
	}
	if size <= maxBytes {
		return nil
	}

	// DELETE alone never shrinks a WAL-mode database file — freed pages stay in
	// the file until a VACUUM rewrites it, so a store that has been pruning for
	// months can be over the cap on free pages alone (the pre-VACUUM code never
	// reclaimed them). Reclaim FIRST and re-measure: eviction before this VACUUM
	// would delete live history — including the snapshot Save just wrote — to
	// free space a rewrite alone would have recovered.
	//
	// VACUUM is best-effort throughout: it fails with SQLITE_BUSY when a sibling
	// handle holds a write transaction (--all-databases opens one Store per
	// goroutine on this same file) and with SQLITE_FULL when the disk can't hold
	// the rewrite. Neither may fail Save — the snapshot row is already
	// committed, and an error here would also drop the schema/events/wait
	// writes that follow. The next run simply retries.
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return nil
	}
	if size, err = s.fileSize(); err != nil || size <= maxBytes {
		return err
	}

	// Still over: evict oldest snapshots in ONE sized pass targeting ~90% of the
	// cap, never touching the newest row. Proportional sizing (overage/filesize
	// of the row count) replaces the old fixed-10%-×-20-rounds loop, which did
	// up to 20 full-file rewrites per call. When snapshots aren't what's over
	// the cap (events and rollups are append-heavy), deleting the history that
	// remains wouldn't help — keeping the newest row bounds the damage and the
	// next run's VACUUM keeps the file as small as it can get.
	var rows int64
	if err := s.db.QueryRow(`SELECT count(*) FROM snapshots`).Scan(&rows); err != nil {
		return err
	}
	if rows <= 1 {
		return nil
	}
	target := maxBytes * 9 / 10
	evict := (rows*(size-target) + size - 1) / size // ceil(rows × overage/size)
	if evict < 1 {
		evict = 1
	}
	if evict > rows-1 {
		evict = rows - 1
	}
	if _, err := s.db.Exec(`
		DELETE FROM snapshots WHERE id IN (
			SELECT id FROM snapshots ORDER BY collected_at ASC LIMIT ?
		)`, evict); err != nil {
		return err
	}
	_, _ = s.db.Exec(`VACUUM`) // best-effort, same reasoning as above
	return nil
}

// fileSize reports the database file size from SQLite's own page accounting.
func (s *Store) fileSize() (int64, error) {
	var pageCount, pageSize int64
	if err := s.db.QueryRow(`PRAGMA page_count`).Scan(&pageCount); err != nil {
		return 0, err
	}
	if err := s.db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, err
	}
	return pageCount * pageSize, nil
}

// scalars are the extracted trend columns.
type scalars struct {
	tps, cacheHit, deadRatioMax, longestXact *float64
	connections, dbSize                      *int64
}

func extractScalars(c *model.Context) scalars {
	var sc scalars
	if c.Health != nil {
		if c.Health.Exactness != model.ExactnessUnavailable {
			sc.tps = c.Health.TPS
			sc.cacheHit = c.Health.CacheHitRatio
		}
		// Counter resets can invalidate rates without losing the current
		// connection gauge. A positive count is still observed in that case.
		// Zero needs availability metadata; legacy/unavailable zero is unknown.
		knownZero := c.Health.Connections == 0 && c.Health.Exactness != "" && c.Health.Exactness != model.ExactnessUnavailable
		if c.Health.Connections > 0 || knownZero {
			n := int64(c.Health.Connections)
			sc.connections = &n
		}
	}
	if c.Tables != nil && c.Tables.Exactness != model.ExactnessUnavailable {
		sc.dbSize = &c.Tables.DBSizeBytes
		var maxDead float64
		for _, t := range c.Tables.Top {
			if t.DeadRatio > maxDead {
				maxDead = t.DeadRatio
			}
		}
		sc.deadRatioMax = &maxDead
	}
	if c.Activity != nil && c.Activity.Exactness != model.ExactnessUnavailable {
		v := c.Activity.LongestXactSec
		sc.longestXact = &v
	}
	return sc
}
