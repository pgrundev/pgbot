package main

import (
	"context"
	"errors"
	"testing"
)

func TestAdviseCommand_ReturnsOutputError(t *testing.T) {
	cmd := newAdviseCmdWithRunner(func(context.Context, string, int, float64) (adviseResult, error) {
		return adviseResult{Database: "app", VersionNum: 160000}, nil
	})
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetOut(rejectingCommandWriter{})
	cmd.SetArgs([]string{"postgres://fixture/app", "--no-color"})

	err := cmd.Execute()
	if !errors.Is(err, errCommandOutput) {
		t.Fatalf("command error = %v, want output error (the root maps this execution failure to exit 3)", err)
	}
}
