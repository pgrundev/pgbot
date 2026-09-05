package collect

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pgrundev/pgbot/internal/conn"
)

// A real two-session lock conflict must produce a SUSTAINED blocker with the
// holder's transaction age, and scrubbed query text throughout. Advisory locks
// create genuine heavyweight-lock contention (visible to pg_blocking_pids)
// without any schema privileges — CI's DSN is a monitoring role that cannot
// CREATE TABLE, exactly pgbot's own recommended posture.
func TestIntegration_waitStudy_namesTheBlocker(t *testing.T) {
	dsn := os.Getenv("PGBOT_TEST_DSN")
	if dsn == "" {
		t.Skip("set PGBOT_TEST_DSN to run integration tests")
	}
	ctx := context.Background()

	// Holder: an open transaction owning the advisory lock.
	holder, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer holder.Close(context.Background())
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(987654321012345)`); err != nil {
		t.Fatalf("acquire advisory lock: %v", err)
	}

	// Victim: blocks on the same advisory lock until the holder rolls back.
	victimDone := make(chan struct{})
	go func() {
		defer close(victimDone)
		v, err := pgx.Connect(ctx, dsn)
		if err != nil {
			return
		}
		defer v.Close(context.Background())
		vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		_, _ = v.Exec(vctx, `SELECT pg_advisory_xact_lock(987654321012345)`)
	}()
	time.Sleep(time.Second) // let the victim start waiting

	target, err := conn.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	target.Warm(ctx)

	study := RunWaitStudy(ctx, target, target.Caps, WaitStudyOptions{Hz: 10, Window: 4 * time.Second})

	_ = tx.Rollback(context.Background()) // release the victim
	<-victimDone

	if study.Profile == nil || !study.Profile.Available {
		t.Fatalf("profile unavailable: %+v", study.Profile)
	}
	if study.Polls-study.PollFailures == 0 || study.LockSnapshots == 0 {
		t.Fatalf("no coverage: %+v", study)
	}
	if len(study.Blockers) == 0 {
		t.Fatalf("sustained blocker not named: blockers=%+v transient=%+v", study.Blockers, study.Transient)
	}
	b := study.Blockers[0]
	if !b.Sustained || b.Observations < 3 {
		t.Errorf("evidence too weak to have been named: %+v", b)
	}
	if b.HolderXactAgeS <= 0 {
		t.Errorf("holder xact age must be observed: %+v", b)
	}
	if len(b.Victims) == 0 || !strings.Contains(b.Victims[0].Query, "pg_advisory_xact_lock") {
		t.Errorf("victim query missing: %+v", b.Victims)
	}
	if strings.Contains(b.Victims[0].Query, "987654321012345") {
		t.Errorf("victim query literal not scrubbed: %q", b.Victims[0].Query)
	}
}
