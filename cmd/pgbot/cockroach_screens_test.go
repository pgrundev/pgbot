package main

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestCockroachScreenCommands(t *testing.T) {
	commands := newCockroachScreenCommands()
	if len(commands) != len(cockroachScreenSpecs) {
		t.Fatalf("got %d commands, want %d", len(commands), len(cockroachScreenSpecs))
	}
	for i, cmd := range commands {
		if got, want := cmd.Name(), cockroachScreenSpecs[i].name; got != want {
			t.Errorf("command %d name = %q, want %q", i, got, want)
		}
		for _, flag := range []string{"no-color", "interval", "timeout", "crdb-admin-url", "crdb-prometheus-url"} {
			if cmd.Flags().Lookup(flag) == nil {
				t.Errorf("%s command missing --%s", cmd.Name(), flag)
			}
		}
	}
}

func TestAICommandsExposeCockroachHealthFlags(t *testing.T) {
	for _, cmd := range []*cobra.Command{newAskCmd(), newExplainCmd()} {
		for _, flag := range []string{"crdb-admin-url", "crdb-prometheus-url"} {
			if cmd.Flags().Lookup(flag) == nil {
				t.Errorf("%s command missing --%s", cmd.Name(), flag)
			}
		}
	}
}
