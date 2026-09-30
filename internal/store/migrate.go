package store

import (
	"database/sql"
	"embed"
	"sort"
	"sync"
)

// Schema is small and forward-only; each migration is idempotent (IF NOT
// EXISTS). SQL lives in migrations/*.sql, applied in filename order.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// SQLite connection setup and idempotent DDL still contend across independent
// handles during parallel first use. Serialize initialization, not normal I/O.
var migrateMu sync.Mutex

func (s *Store) migrate() error {
	migrateMu.Lock()
	defer migrateMu.Unlock()

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		stmt, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if _, err := s.db.Exec(string(stmt)); err != nil {
			return err
		}
	}
	return s.migrateFingerprintScheme()
}

// migrateFingerprintScheme records the fingerprint key version. v2 is
// per-database within a cluster; v1 keyed on the cluster-wide system identifier
// alone and collided across databases (the P0-1 bug). Old snapshots cannot be
// recomputed — the system identifier is not stored in a snapshot — so v1 series
// are left in place (baselines list still shows them by database name) but will
// not match new per-database runs. We flag the transition once, and only when
// there is pre-upgrade history, so the CLI can tell the user their series reset.
func (s *Store) migrateFingerprintScheme() error {
	var scheme string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'fingerprint_scheme'`).Scan(&scheme)
	if err == nil {
		return nil // already recorded — nothing to do
	}
	if err != sql.ErrNoRows {
		return err
	}
	var existing int
	if err := s.db.QueryRow(`SELECT count(*) FROM snapshots`).Scan(&existing); err != nil {
		return err
	}
	res, err := s.db.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES ('fingerprint_scheme', '2')`)
	if err != nil {
		return err
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if inserted > 0 && existing > 0 {
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO meta(key, value) VALUES ('fingerprint_notice', '1')`); err != nil {
			return err
		}
	}
	return nil
}

// UpgradeNotice returns a one-time, human-facing message (and clears it) when
// this store carried pre-upgrade history across the v1→v2 fingerprint fix.
// Returns "" otherwise. Callers print it once on a normal run.
func (s *Store) UpgradeNotice() string {
	var v string
	if err := s.db.QueryRow(`DELETE FROM meta WHERE key = 'fingerprint_notice' RETURNING value`).Scan(&v); err != nil {
		return ""
	}
	return "baseline fingerprints are now per-database within a cluster (a bug fix). " +
		"Snapshots taken before this upgrade used a cluster-wide key and won't match new runs; " +
		"clear the old ones with `pgbot baselines prune <fingerprint>` if you don't need them."
}

// allowedScalar guards the one place a column name is interpolated into SQL
// (Trend): only these names are ever accepted.
var allowedScalar = map[string]bool{
	"tps": true, "cache_hit": true, "connections": true,
	"db_size_bytes": true, "dead_ratio_max": true, "longest_xact_s": true,
}
