package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMCPCmdSuccessfulEOFDeregistersInputClose(t *testing.T) {
	in := &trackingReadCloser{
		Reader: strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"),
		closed: make(chan struct{}),
	}
	cmd := newMCPCmd()
	cmd.SetIn(in)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), `{"jsonrpc":"2.0","id":1,"result":{}}`+"\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if !strings.Contains(stderr.String(), "pgbot mcp: serving on stdio") {
		t.Fatalf("startup banner was not routed to command stderr: %q", stderr.String())
	}

	cancel()
	select {
	case <-in.closed:
		t.Fatal("later context cancellation closed input after Serve returned on EOF")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestMCPCmdCancellationClosesActiveInputAndJoinsCallback(t *testing.T) {
	in := newBlockingReadCloser()
	t.Cleanup(in.releaseClose)
	cmd := newMCPCmd()
	cmd.SetIn(in)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := make(chan error, 1)
	go func() { result <- cmd.ExecuteContext(ctx) }()

	select {
	case <-in.readStarted:
	case err := <-result:
		t.Fatalf("MCP command returned before reading its configured input: %v", err)
	case <-time.After(time.Second):
		t.Fatal("MCP command did not start reading its configured input")
	}

	cancel()
	select {
	case <-in.closeStarted:
	case <-time.After(time.Second):
		in.releaseClose()
		t.Fatal("cancellation did not close the active input")
	}
	select {
	case <-in.readReturned:
	case <-time.After(time.Second):
		in.releaseClose()
		t.Fatal("closing the input did not unblock the active read")
	}
	select {
	case err := <-result:
		in.releaseClose()
		t.Fatalf("MCP command returned before its input-close callback completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	in.releaseClose()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("MCP command error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("MCP command did not return after its input-close callback completed")
	}
}

type trackingReadCloser struct {
	io.Reader
	closed chan struct{}
	once   sync.Once
}

func (r *trackingReadCloser) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

type blockingReadCloser struct {
	readStarted  chan struct{}
	readReturned chan struct{}
	closeStarted chan struct{}
	closeRelease chan struct{}
	readOnce     sync.Once
	closeOnce    sync.Once
	releaseOnce  sync.Once
}

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{
		readStarted:  make(chan struct{}),
		readReturned: make(chan struct{}),
		closeStarted: make(chan struct{}),
		closeRelease: make(chan struct{}),
	}
}

func (r *blockingReadCloser) Read([]byte) (int, error) {
	r.readOnce.Do(func() { close(r.readStarted) })
	<-r.closeStarted
	close(r.readReturned)
	return 0, io.ErrClosedPipe
}

func (r *blockingReadCloser) Close() error {
	r.closeOnce.Do(func() { close(r.closeStarted) })
	<-r.closeRelease
	return nil
}

func (r *blockingReadCloser) releaseClose() {
	r.releaseOnce.Do(func() { close(r.closeRelease) })
}
