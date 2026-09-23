package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

func TestOpenConcurrentFingerprintMigration(t *testing.T) {
	const peers = 16
	openTogether := func(path string) []error {
		t.Helper()
		start := make(chan struct{})
		errs := make(chan error, peers)
		var wg sync.WaitGroup
		for i := 0; i < peers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				st, err := Open(path)
				if err == nil {
					err = st.Close()
				}
				errs <- err
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		var out []error
		for err := range errs {
			if err != nil {
				out = append(out, err)
			}
		}
		return out
	}

	t.Run("fresh store", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "fresh.db")
		if errs := openTogether(path); len(errs) > 0 {
			t.Fatalf("%d of %d concurrent Open calls failed; first error: %v", len(errs), peers, errs[0])
		}
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if notice := st.UpgradeNotice(); notice != "" {
			t.Errorf("fresh store must not get a legacy migration notice: %q", notice)
		}
	})

	t.Run("legacy store with history", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "legacy.db")
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		c := &model.Context{
			SchemaVersion: model.SchemaVersion,
			Fingerprint:   "legacy",
			CollectedAt:   time.Now().UTC(),
		}
		if _, err := st.Save(c); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.Exec(`DELETE FROM meta WHERE key IN ('fingerprint_scheme', 'fingerprint_notice')`); err != nil {
			t.Fatal(err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}

		if errs := openTogether(path); len(errs) > 0 {
			t.Fatalf("%d of %d concurrent Open calls failed; first error: %v", len(errs), peers, errs[0])
		}
		st, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if notice := st.UpgradeNotice(); notice == "" {
			t.Error("legacy history must retain the one-time fingerprint migration notice")
		}
		if notice := st.UpgradeNotice(); notice != "" {
			t.Errorf("migration notice must still be one-time, got %q", notice)
		}
		var snapshots int
		if err := st.db.QueryRow(`SELECT count(*) FROM snapshots WHERE fingerprint = 'legacy'`).Scan(&snapshots); err != nil || snapshots != 1 {
			t.Fatalf("migration changed the existing snapshot: count=%d error=%v", snapshots, err)
		}
		var scheme string
		if err := st.db.QueryRow(`SELECT value FROM meta WHERE key = 'fingerprint_scheme'`).Scan(&scheme); err != nil {
			t.Fatal(err)
		}
		if scheme != "2" {
			t.Errorf("fingerprint scheme = %q, want 2", scheme)
		}
	})
}

func TestUpgradeNoticeConcurrentConsumption(t *testing.T) {
	const peers = 16
	path := filepath.Join(t.TempDir(), "notice.db")
	stores := make([]*Store, peers)
	for i := range stores {
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = st
		t.Cleanup(func() { _ = st.Close() })
	}
	if _, err := stores[0].db.Exec(`INSERT INTO meta(key,value) VALUES ('fingerprint_notice','1')`); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan string, peers)
	for _, st := range stores {
		go func(st *Store) { <-start; results <- st.UpgradeNotice() }(st)
	}
	close(start)
	notices := 0
	for range stores {
		if <-results != "" {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("one durable notice must be consumed exactly once; got %d deliveries", notices)
	}
	if got := stores[0].UpgradeNotice(); got != "" {
		t.Fatalf("notice reappeared: %q", got)
	}
}
