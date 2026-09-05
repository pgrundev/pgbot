package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
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

// Every collection command built on inspectFlags exposes --timeout with the same
// 30s default, so `--timeout 60s` from the docs works uniformly. tune was the
// one that lacked it (#26); keep the set in step.
func TestCollectionCommandsShareTimeoutDefault(t *testing.T) {
	cmds := map[string]func() *cobra.Command{
		"tune":    newTuneCmd,
		"indexes": newIndexesCmd,
		"queries": newQueriesCmd,
		"tables":  newTablesCmd,
		"vacuum":  newVacuumCmd,
		"inspect": newInspectCmd,
	}
	for name, build := range cmds {
		fl := build().Flags().Lookup("timeout")
		if fl == nil {
			t.Errorf("%s: no --timeout flag", name)
			continue
		}
		if fl.DefValue != "30s" {
			t.Errorf("%s: --timeout default = %q; want \"30s\"", name, fl.DefValue)
		}
	}
}

func TestTuneTimeoutFlagParsesDurations(t *testing.T) {
	cmd := newTuneCmd()
	if err := cmd.Flags().Parse([]string{"--timeout=2m30s"}); err != nil {
		t.Fatal(err)
	}
	got, err := cmd.Flags().GetDuration("timeout")
	if err != nil {
		t.Fatal(err)
	}
	if want := 150 * time.Second; got != want {
		t.Fatalf("--timeout=2m30s parsed as %s; want %s", got, want)
	}
	if err := newTuneCmd().Flags().Parse([]string{"--timeout=soon"}); err == nil {
		t.Fatal("--timeout=soon parsed without error; want a duration syntax error")
	}
}
