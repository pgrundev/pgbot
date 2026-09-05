package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func responsesFor(url, key, model string) LanguageModel {
	p := &ResponsesProvider{APIKey: key, BaseURL: url, HTTP: http.DefaultClient, Label: "xai"}
	m, _ := p.LanguageModel(context.Background(), model)
	return m
}

// The shape below mirrors a real grok-4.6 response: a reasoning entry the user
// must never see, followed by the message.
func TestResponses_success(t *testing.T) {
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer k123" {
			t.Errorf("key must travel in the Authorization header, got %q", got)
		}
		if strings.Contains(r.URL.String(), "k123") {
			t.Error("API key leaked into the URL")
		}
		if r.URL.Path != "/responses" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &raw); err != nil {
			t.Fatalf("bad request body: %v", err)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status": "completed",
			"output": []map[string]any{
				{"type": "reasoning", "summary": []map[string]string{{"type": "summary_text", "text": "internal"}}},
				{"type": "message", "role": "assistant", "content": []map[string]string{
					{"type": "output_text", "text": "Looks healthy."},
				}},
			},
		})
	}))
	defer srv.Close()

	out, err := responsesFor(srv.URL, "k123", "grok-4.6").Generate(context.Background(), Call{
		System: "be terse", Prompt: "the report", MaxOutputTokens: i64(8192),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "Looks healthy." {
		t.Errorf("unexpected output %q — reasoning entries must not leak in", out.Text)
	}
	if raw["instructions"] != "be terse" || raw["input"] != "the report" {
		t.Errorf("system/user must map to instructions/input, got %v / %v", raw["instructions"], raw["input"])
	}
	// The API defaults store to true, which would retain the findings server-side.
	if raw["store"] != false {
		t.Errorf("store must be explicitly false, got %v", raw["store"])
	}
}

// xAI returns a bare string under "error" where OpenAI returns an object.
// Decoding it as a struct loses the message the user needs.
func TestResponses_xaiStringError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"code":"invalid-argument","error":"Incorrect API key provided. You can obtain an API key from https://console.x.ai."}`))
	}))
	defer srv.Close()

	_, err := responsesFor(srv.URL, "bad", "grok-4.6").Generate(context.Background(), Call{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "Incorrect API key provided") {
		t.Errorf("xAI's string error must be surfaced verbatim, got %v", err)
	}
}

func TestResponses_openAIObjectError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"Invalid Authentication","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	_, err := responsesFor(srv.URL, "bad", "gpt-5.6-terra").Generate(context.Background(), Call{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "Invalid Authentication") {
		t.Errorf("OpenAI's object error must be surfaced too, got %v", err)
	}
}

// Reasoning can consume the whole budget, leaving a message entry with no text.
func TestResponses_truncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"status":             "incomplete",
			"incomplete_details": map[string]string{"reason": "max_output_tokens"},
			"output":             []map[string]any{{"type": "reasoning"}},
		})
	}))
	defer srv.Close()

	_, err := responsesFor(srv.URL, "k", "grok-4.6").Generate(context.Background(), Call{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "output cap") {
		t.Errorf("a truncated response should explain the cause, got %v", err)
	}
}

func TestResponses_reasoningEffortOmittedUnlessSet(t *testing.T) {
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &raw)
		json.NewEncoder(w).Encode(map[string]any{
			"status": "completed",
			"output": []map[string]any{{"type": "message", "content": []map[string]string{{"type": "output_text", "text": "ok"}}}},
		})
	}))
	defer srv.Close()

	if _, err := responsesFor(srv.URL, "k", "grok-4.6").Generate(context.Background(), Call{Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["reasoning"]; ok {
		t.Error("reasoning must be omitted unless an effort is configured — vocabularies differ per vendor")
	}

	p := &ResponsesProvider{APIKey: "k", BaseURL: srv.URL, HTTP: http.DefaultClient, Label: "xai", ReasoningEffort: "low"}
	m, _ := p.LanguageModel(context.Background(), "grok-4.6")
	if _, err := m.Generate(context.Background(), Call{Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	got, _ := raw["reasoning"].(map[string]any)
	if got == nil || got["effort"] != "low" {
		t.Errorf("configured effort should be sent, got %v", raw["reasoning"])
	}
}

func TestResolve_xai(t *testing.T) {
	clearEnv(t)
	t.Setenv("XAI_API_KEY", "k")
	m, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if m.Provider() != "xai" {
		t.Errorf("provider = %q, want xai", m.Provider())
	}
	if m.Model() != defaultXAIModel {
		t.Errorf("model = %q, want %q", m.Model(), defaultXAIModel)
	}
	if m.Endpoint() != defaultXAIURL {
		t.Errorf("endpoint = %q, want %q", m.Endpoint(), defaultXAIURL)
	}
}

// PGBOT_AI_PROVIDER=responses with an OpenAI key must not inherit Grok's model.
func TestResolve_responsesWithOpenAIKey(t *testing.T) {
	clearEnv(t)
	t.Setenv("PGBOT_AI_PROVIDER", "responses")
	t.Setenv("OPENAI_API_KEY", "k")
	m, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if m.Provider() != "openai" {
		t.Errorf("provider = %q, want openai", m.Provider())
	}
	if m.Model() != defaultOpenAIModel {
		t.Errorf("model = %q, want %q", m.Model(), defaultOpenAIModel)
	}
	if m.Endpoint() != defaultOpenAIURL {
		t.Errorf("endpoint = %q, want %q", m.Endpoint(), defaultOpenAIURL)
	}
}

// An existing OpenAI/Ollama setup must keep the chat/completions provider —
// /responses is not implemented by the local runtimes.
func TestResolve_openAIKeyStillUsesChatCompletions(t *testing.T) {
	clearEnv(t)
	t.Setenv("OPENAI_API_KEY", "k")
	m, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.(*openaiModel); !ok {
		t.Errorf("auto-detected OpenAI must use the chat/completions provider, got %T", m)
	}
}
