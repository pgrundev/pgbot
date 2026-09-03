package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestTuneTimeoutBoundsRun(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		<-done
	}()
	t.Cleanup(func() {
		close(done)
		_ = listener.Close()
	})

	dsn := fmt.Sprintf("postgres://pgbot:secret@%s/postgres?sslmode=disable", listener.Addr())
	cmd := newTuneCmd()
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{dsn, "--timeout=50ms"})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	err = cmd.ExecuteContext(ctx)
	elapsed := time.Since(started)

	if err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("tune --timeout error = %v; want context deadline exceeded", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("tune --timeout=50ms returned after %s; want it to bound the run", elapsed)
	}
}

func TestGatherOptionsForwardsTimeout(t *testing.T) {
	want := 2 * time.Minute
	got := gatherOptions(inspectFlags{timeout: want})

	if got.Deadline != want {
		t.Fatalf("gather collection deadline = %s; want %s", got.Deadline, want)
	}
}
