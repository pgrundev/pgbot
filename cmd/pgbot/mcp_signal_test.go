package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

const mcpMainHelperEnv = "PGBOT_TEST_MCP_MAIN"

func TestMCPMainHelper(t *testing.T) {
	if os.Getenv(mcpMainHelperEnv) != "1" {
		return
	}
	os.Args = []string{"pgbot", "mcp"}
	main()
}

func TestMCPMainExitsSuccessfullyOnEOF(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMCPMainHelper$")
	child.Env = append(os.Environ(), mcpMainHelperEnv+"=1")
	child.Stdin = strings.NewReader("")

	output, err := child.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("MCP process did not finish after EOF: %v", ctx.Err())
	}
	if err != nil {
		t.Fatalf("MCP process returned an error after EOF: %v; output: %q", err, output)
	}
	if !bytes.Contains(output, []byte("pgbot mcp: serving on stdio")) {
		t.Fatalf("MCP startup banner missing after normal EOF: %q", output)
	}
}

func TestMCPMainStopsOnSIGTERMWhileInputIsIdle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal delivery is not available on Windows")
	}

	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdinReader.Close()
		_ = stdinWriter.Close()
	})

	child := exec.Command(os.Args[0], "-test.run=^TestMCPMainHelper$")
	child.Env = append(os.Environ(), mcpMainHelperEnv+"=1")
	child.Stdin = stdinReader
	var stdout bytes.Buffer
	child.Stdout = &stdout
	stderr, err := child.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	_ = stdinReader.Close()

	ready := make(chan struct{}, 1)
	stderrDone := make(chan string, 1)
	go func() {
		var lines []string
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			lines = append(lines, line)
			if strings.Contains(line, "pgbot mcp: serving on stdio") {
				select {
				case ready <- struct{}{}:
				default:
				}
			}
		}
		if scanErr := scanner.Err(); scanErr != nil {
			lines = append(lines, "stderr read error: "+scanErr.Error())
		}
		stderrDone <- strings.Join(lines, "\n")
	}()

	waitDone := make(chan error, 1)
	go func() { waitDone <- child.Wait() }()
	killAndWait := func() (string, error) {
		_ = child.Process.Kill()
		waitErr := <-waitDone
		return <-stderrDone, waitErr
	}

	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		stderrText, waitErr := killAndWait()
		t.Fatalf("MCP server did not report readiness; wait error: %v; stderr: %q", waitErr, stderrText)
	}

	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		stderrText, waitErr := killAndWait()
		t.Fatalf("send SIGTERM: %v; wait error: %v; stderr: %q", err, waitErr, stderrText)
	}

	select {
	case waitErr := <-waitDone:
		stderrText := <-stderrDone
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != exitFailure {
			t.Fatalf("after SIGTERM, exit error = %v, want exit code %d; stderr: %q; stdout: %q", waitErr, exitFailure, stderrText, stdout.String())
		}
	case <-time.After(2 * time.Second):
		stderrText, waitErr := killAndWait()
		t.Fatalf("MCP server did not exit after SIGTERM while stdin remained open; cleanup wait error: %v; stderr: %q", waitErr, stderrText)
	}
}
