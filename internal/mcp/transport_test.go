package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

type transportWriterFunc func([]byte) (int, error)

func (f transportWriterFunc) Write(p []byte) (int, error) { return f(p) }

func TestServe_returnsWriteErrorAndStopsDispatch(t *testing.T) {
	brokenPipe := errors.New("output pipe closed")
	for _, tc := range []struct {
		name    string
		payload string
		writer  io.Writer
		wantErr error
	}{
		{
			name:    "buffered response",
			payload: "ok",
			writer:  transportWriterFunc(func([]byte) (int, error) { return 0, brokenPipe }),
			wantErr: brokenPipe,
		},
		{
			name:    "response larger than buffer",
			payload: strings.Repeat("x", 8192),
			writer:  transportWriterFunc(func([]byte) (int, error) { return 0, brokenPipe }),
			wantErr: brokenPipe,
		},
		{
			name:    "short write",
			payload: "ok",
			writer:  transportWriterFunc(func(p []byte) (int, error) { return len(p) / 2, nil }),
			wantErr: io.ErrShortWrite,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := &Server{Tools: []Tool{{
				Name: "inspect",
				Handler: func(context.Context, json.RawMessage) (string, error) {
					calls++
					return tc.payload, nil
				},
			}}}
			in := strings.NewReader(
				`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"inspect"}}` + "\n" +
					`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"inspect"}}` + "\n")
			err := srv.Serve(context.Background(), in, tc.writer)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Serve error = %v, want %v", err, tc.wantErr)
			}
			if calls != 1 {
				t.Errorf("dispatched %d tools after output failed; want only the first tool", calls)
			}
		})
	}
}

func TestServe_finalResponseWriteErrorWinsOverEOF(t *testing.T) {
	wantErr := errors.New("output pipe closed")
	out := transportWriterFunc(func([]byte) (int, error) { return 0, wantErr })
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if err := testServer().Serve(context.Background(), in, out); !errors.Is(err, wantErr) {
		t.Fatalf("Serve error = %v, want output failure rather than successful EOF", err)
	}
}
