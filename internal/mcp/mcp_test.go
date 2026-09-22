package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func testServer() *Server {
	return &Server{
		Name: "pgbot", Version: "test",
		Tools: []Tool{{
			Name: "echo", Description: "echoes back", InputSchema: map[string]any{"type": "object"},
			Handler: func(_ context.Context, args json.RawMessage) (string, error) {
				return "got " + string(args), nil
			},
		}},
	}
}

// decode splits the NDJSON transcript into parsed JSON-RPC responses.
func decode(t *testing.T, out string) []map[string]any {
	t.Helper()
	var msgs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad response line %q: %v", line, err)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func TestServe_handshakeListAndCall(t *testing.T) {
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`, // notification — no reply
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"x":1}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope"}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := testServer().Serve(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	msgs := decode(t, out.String())

	// 4 requests carry ids; the notification gets no reply → exactly 4 responses.
	if len(msgs) != 4 {
		t.Fatalf("want 4 responses (notification is silent), got %d:\n%s", len(msgs), out.String())
	}

	// initialize: echoes the client's protocol revision and names the server.
	init := msgs[0]
	res := init["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" {
		t.Errorf("initialize should echo client protocol, got %v", res["protocolVersion"])
	}
	if res["serverInfo"].(map[string]any)["name"] != "pgbot" {
		t.Error("serverInfo.name should be pgbot")
	}

	// tools/list: the echo tool with its schema.
	tools := msgs[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "echo" {
		t.Errorf("tools/list wrong: %v", tools)
	}

	// tools/call echo: text content, not an error.
	call := msgs[2]["result"].(map[string]any)
	if call["isError"] == true {
		t.Error("echo call should not be an error")
	}
	text := call["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "got ") {
		t.Errorf("unexpected tool text: %q", text)
	}

	// unknown tool: a JSON-RPC error.
	if _, ok := msgs[3]["error"]; !ok {
		t.Errorf("unknown tool should return a JSON-RPC error, got %v", msgs[3])
	}
}

func TestServe_promptsAndResources(t *testing.T) {
	srv := &Server{
		Name: "pgbot", Version: "test",
		Prompts: []Prompt{{
			Name: "diagnose", Description: "diagnose it",
			Arguments: []PromptArg{{Name: "connection_string"}},
			Build: func(_ context.Context, args map[string]string) ([]PromptMessage, error) {
				return []PromptMessage{{Role: "user", Text: "inspect " + args["connection_string"]}}, nil
			},
		}},
		Resources: []Resource{{
			URI: "pgbot://baselines", Name: "baselines", MimeType: "application/json",
			Read: func(context.Context) (string, error) { return `[{"database":"app"}]`, nil },
		}},
	}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"prompts/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"prompts/get","params":{"name":"diagnose","arguments":{"connection_string":"pg://x"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"resources/list"}`,
		`{"jsonrpc":"2.0","id":5,"method":"resources/read","params":{"uri":"pgbot://baselines"}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	m := decode(t, out.String())

	// initialize advertises prompts + resources capabilities.
	caps := m[0]["result"].(map[string]any)["capabilities"].(map[string]any)
	if _, ok := caps["prompts"]; !ok {
		t.Error("initialize should advertise the prompts capability")
	}
	if _, ok := caps["resources"]; !ok {
		t.Error("initialize should advertise the resources capability")
	}
	// prompts/get renders with the argument.
	msgs := m[2]["result"].(map[string]any)["messages"].([]any)
	txt := msgs[0].(map[string]any)["content"].(map[string]any)["text"].(string)
	if !strings.Contains(txt, "pg://x") {
		t.Errorf("prompt should interpolate the argument, got %q", txt)
	}
	// resources/read returns the content for the URI.
	contents := m[4]["result"].(map[string]any)["contents"].([]any)
	if got := contents[0].(map[string]any)["text"].(string); !strings.Contains(got, "app") {
		t.Errorf("resource read wrong: %q", got)
	}
}

func TestServe_toolErrorIsResultNotTransportError(t *testing.T) {
	srv := &Server{Name: "pgbot", Version: "test", Tools: []Tool{{
		Name: "boom", Handler: func(context.Context, json.RawMessage) (string, error) {
			return "", context.DeadlineExceeded
		},
	}}}
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"boom"}}` + "\n"
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	m := decode(t, out.String())[0]
	// A tool failure is a result with isError=true, so the session keeps going.
	if _, isRPCErr := m["error"]; isRPCErr {
		t.Error("a tool failure must be a tool result, not a JSON-RPC transport error")
	}
	if m["result"].(map[string]any)["isError"] != true {
		t.Error("tool failure result should have isError=true")
	}
}

func TestServe_preCanceledContextDoesNotDispatch(t *testing.T) {
	called := false
	srv := &Server{Name: "pgbot", Version: "test", Tools: []Tool{{
		Name: "must-not-run", Handler: func(context.Context, json.RawMessage) (string, error) {
			called = true
			return "unexpected", nil
		},
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"must-not-run"}}` + "\n")
	var out bytes.Buffer

	err := srv.Serve(ctx, in, &out)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("tool was dispatched with an already-canceled context")
	}
	if out.Len() != 0 {
		t.Fatalf("canceled request produced output %q", out.String())
	}
}

func TestServe_cancellationBetweenBufferedRequestsStopsDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	secondCalled := false
	srv := &Server{Name: "pgbot", Version: "test", Tools: []Tool{
		{
			Name: "first", Handler: func(context.Context, json.RawMessage) (string, error) {
				cancel()
				return "first completed", nil
			},
		},
		{
			Name: "second", Handler: func(context.Context, json.RawMessage) (string, error) {
				secondCalled = true
				return "unexpected", nil
			},
		},
	}}
	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"first"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"second"}}`,
	}, "\n") + "\n")
	var out bytes.Buffer

	err := srv.Serve(ctx, in, &out)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve error = %v, want context.Canceled", err)
	}
	if secondCalled {
		t.Fatal("second buffered request was dispatched after cancellation")
	}
	msgs := decode(t, out.String())
	if len(msgs) != 1 || msgs[0]["id"] != float64(1) {
		t.Fatalf("responses = %#v, want only request 1", msgs)
	}
}

func TestServe_preservesReadError(t *testing.T) {
	wantErr := errors.New("read failed")
	err := testServer().Serve(context.Background(), errorReader{err: wantErr}, io.Discard)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Serve error = %v, want %v", err, wantErr)
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }
