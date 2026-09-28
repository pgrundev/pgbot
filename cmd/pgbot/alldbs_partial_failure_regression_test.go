package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

const allDatabasesHelperEnv = "PGBOT_TEST_ALL_DATABASES_PROCESS"

func TestAllDatabasesPartialFailureExitsThreeAndKeepsSuccessfulOutput(t *testing.T) {
	if os.Getenv(allDatabasesHelperEnv) == "1" {
		runAllDatabasesCLIProcessFixture()
		return
	}

	tests := []struct {
		name       string
		partial    bool
		allFailed  bool
		severity   string
		failOn     string
		wantCode   int
		wantReport bool
	}{
		{name: "partial clean", partial: true, failOn: "warn", wantCode: exitFailure, wantReport: true},
		{name: "partial with findings disabled", partial: true, failOn: "none", wantCode: exitFailure, wantReport: true},
		{name: "partial warning", partial: true, severity: model.SeverityWarn, failOn: "critical", wantCode: exitFailure, wantReport: true},
		{name: "partial critical", partial: true, severity: model.SeverityCritical, failOn: "warn", wantCode: exitFailure, wantReport: true},
		{name: "complete clean", failOn: "warn", wantCode: exitClean, wantReport: true},
		{name: "complete warning", severity: model.SeverityWarn, failOn: "warn", wantCode: exitWarn, wantReport: true},
		{name: "complete critical", severity: model.SeverityCritical, failOn: "warn", wantCode: exitCritical, wantReport: true},
		{name: "warning below user threshold", severity: model.SeverityWarn, failOn: "critical", wantCode: exitClean, wantReport: true},
		{name: "all failed", allFailed: true, failOn: "warn", wantCode: exitFailure},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAllDatabasesPartialFailureExitsThreeAndKeepsSuccessfulOutput$")
			cmd.Env = append(os.Environ(),
				allDatabasesHelperEnv+"=1",
				"PGBOT_TEST_ALL_DATABASES_PARTIAL="+fmt.Sprint(tt.partial),
				"PGBOT_TEST_ALL_DATABASES_ALL_FAILED="+fmt.Sprint(tt.allFailed),
				"PGBOT_TEST_ALL_DATABASES_SEVERITY="+tt.severity,
				"PGBOT_TEST_ALL_DATABASES_FAIL_ON="+tt.failOn,
			)
			var stdout, stderr strings.Builder
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			if got := allDatabasesProcessExitCode(err); got != tt.wantCode {
				t.Fatalf("exit code = %d, want %d; error=%v; stderr=%q; stdout=%q", got, tt.wantCode, err, stderr.String(), stdout.String())
			}
			if !tt.wantReport {
				if stdout.Len() != 0 || !strings.Contains(stderr.String(), "every database failed to inspect") {
					t.Fatalf("all-failed output: stderr=%q stdout=%q", stderr.String(), stdout.String())
				}
				return
			}

			var report []*model.Context
			if err := json.Unmarshal([]byte(stdout.String()), &report); err != nil {
				t.Fatalf("successful database output is not JSON: %v; output=%q", err, stdout.String())
			}
			if len(report) != 1 || report[0].Server.Database != "working" {
				t.Fatalf("successful database output = %#v, want only working database", report)
			}
			if tt.partial && !strings.Contains(stderr.String(), "skipping broken") {
				t.Fatalf("partial failure diagnostic missing from stderr: %q", stderr.String())
			}
		})
	}
}

func runAllDatabasesCLIProcessFixture() {
	partial := os.Getenv("PGBOT_TEST_ALL_DATABASES_PARTIAL") == "true"
	allFailed := os.Getenv("PGBOT_TEST_ALL_DATABASES_ALL_FAILED") == "true"
	severity := os.Getenv("PGBOT_TEST_ALL_DATABASES_SEVERITY")
	failOn := os.Getenv("PGBOT_TEST_ALL_DATABASES_FAIL_ON")
	dbs := []string{"working"}
	if partial {
		dbs = append(dbs, "broken")
	}
	if allFailed {
		dbs = []string{"broken-a", "broken-b"}
	}
	allDatabasesList = func(context.Context, string) ([]string, error) { return dbs, nil }
	allDatabasesInspect = func(_ context.Context, _, database string, _ inspectFlags) (*model.Context, error) {
		if strings.HasPrefix(database, "broken") {
			return nil, errors.New("fixture inspection failure")
		}
		c := &model.Context{
			SchemaVersion: model.SchemaVersion,
			Fingerprint:   "fixture-" + database,
			Server:        model.ServerInfo{Database: database},
		}
		if severity != "" {
			c.Findings = []model.Finding{{ID: "fixture_finding", Severity: severity}}
		}
		return c, nil
	}
	os.Args = []string{
		os.Args[0], "inspect", "fixture", "--all-databases", "--no-store",
		"--format=json", "--fail-on=" + failOn, "--parallel=2",
	}
	main()
	panic("pgbot CLI returned without exiting")
}

func allDatabasesProcessExitCode(err error) int {
	if err == nil {
		return exitClean
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
