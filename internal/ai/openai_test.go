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

func openaiFor(url, key, model string) LanguageModel {
	p := &OpenAIProvider{APIKey: key, BaseURL: url, HTTP: http.DefaultClient}
	m, _ := p.LanguageModel(context.Background(), model)
	return m
}

func TestOpenAI_success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer k123" {
			t.Errorf("key must travel in the Authorization header, got %q", got)
		}
		if strings.Contains(r.URL.String(), "k123") {
			t.Error("API key leaked into the URL")
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req chatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("bad request body: %v", err)
		}
		if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
			t.Errorf("request shape wrong: %+v", req.Messages)
		}
		if req.Model != "m1" {
			t.Errorf("model not sent: %q", req.Model)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": "Looks healthy."},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	out, err := openaiFor(srv.URL, "k123", "m1").Generate(context.Background(), Call{System: "be terse", Prompt: "the report"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "Looks healthy." {
		t.Errorf("unexpected output %q", out.Text)
	}
}

func TestOpenAI_apiError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"message": "Incorrect API key provided", "type": "invalid_request_error"},
		})
	}))
	defer srv.Close()

	_, err := openaiFor(srv.URL, "bad", "m1").Generate(context.Background(), Call{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "Incorrect API key provided") {
		t.Errorf("expected a surfaced API error, got %v", err)
	}
}

func TestOpenAI_noChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{}}) // 200, nothing to say
	}))
	defer srv.Close()
	if _, err := openaiFor(srv.URL, "k", "m").Generate(context.Background(), Call{Prompt: "x"}); err == nil {
		t.Error("no choices should be an error, not an empty success")
	}
}

// OpenAI's reasoning models reject max_tokens outright and take a reasoning
// effort. Getting this wrong 400s the *default* configuration, so pin the shape.
func TestOpenAI_reasoningRequestShape(t *testing.T) {
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &raw); err != nil {
			t.Fatalf("bad request body: %v", err)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "ok"}}},
		})
	}))
	defer srv.Close()

	p := &OpenAIProvider{APIKey: "k", BaseURL: srv.URL, HTTP: http.DefaultClient}
	m, _ := p.LanguageModel(context.Background(), defaultOpenAIModel)
	if _, err := m.Generate(context.Background(), Call{
		Prompt: "x", Temperature: f64(0.2), MaxOutputTokens: i64(8192),
	}); err != nil {
		t.Fatal(err)
	}

	if _, ok := raw["max_tokens"]; ok {
		t.Error("max_tokens must not be sent to a reasoning model — it is rejected")
	}
	if _, ok := raw["temperature"]; ok {
		t.Error("temperature must not be sent to a reasoning model")
	}
	if raw["reasoning_effort"] != defaultReasoningEffort {
		t.Errorf("reasoning_effort = %v, want %q", raw["reasoning_effort"], defaultReasoningEffort)
	}
	// The cap covers hidden reasoning too, so the 8192 the caller asked for must
	// be raised rather than passed through.
	if got, ok := raw["max_completion_tokens"].(float64); !ok || int(got) != reasoningTokenFloor {
		t.Errorf("max_completion_tokens = %v, want the reasoning floor %d", raw["max_completion_tokens"], reasoningTokenFloor)
	}
}

// A non-reasoning model — including everything served by a local runtime — must
// keep the plain shape, or Ollama/vLLM stop working.
func TestOpenAI_plainModelKeepsLegacyShape(t *testing.T) {
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &raw)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "ok"}}},
		})
	}))
	defer srv.Close()

	_, err := openaiFor(srv.URL, "", "llama3.1").Generate(context.Background(), Call{
		Prompt: "x", Temperature: f64(0.2), MaxOutputTokens: i64(8192),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["max_completion_tokens"]; ok {
		t.Error("max_completion_tokens must not be sent to a plain chat model")
	}
	if _, ok := raw["reasoning_effort"]; ok {
		t.Error("reasoning_effort must not be sent to a plain chat model")
	}
	if got, ok := raw["max_tokens"].(float64); !ok || int(got) != 8192 {
		t.Errorf("max_tokens = %v, want 8192", raw["max_tokens"])
	}
	if raw["temperature"] == nil {
		t.Error("temperature should still be sent to a plain chat model")
	}
}

func TestReasoningModelDetection(t *testing.T) {
	reasoning := []string{"gpt-5.6-terra", "gpt-5.6-sol", "GPT-5.6-Luna", "openai/gpt-5.6-terra", "o3-mini", "o4-mini"}
	plain := []string{"gpt-4o-mini", "llama3.1", "qwen2.5:7b-instruct", "mistral-large", "deepseek-chat"}
	for _, id := range reasoning {
		if !reasoningModel(id) {
			t.Errorf("%q should be detected as a reasoning model", id)
		}
	}
	for _, id := range plain {
		if reasoningModel(id) {
			t.Errorf("%q should NOT be detected as a reasoning model", id)
		}
	}
}

func TestOpenAI_reasoningEffortOverride(t *testing.T) {
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &raw)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "ok"}}},
		})
	}))
	defer srv.Close()

	p := &OpenAIProvider{APIKey: "k", BaseURL: srv.URL, HTTP: http.DefaultClient, ReasoningEffort: "low"}
	m, _ := p.LanguageModel(context.Background(), "gpt-5.6-terra")
	if _, err := m.Generate(context.Background(), Call{Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	if raw["reasoning_effort"] != "low" {
		t.Errorf("reasoning_effort = %v, want the configured override", raw["reasoning_effort"])
	}
}

// Small local reasoning models routinely spend the whole cap on hidden thinking
// and return empty content — the error has to say what to do about it.
func TestOpenAI_reasoningAteTheBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"content": "", "reasoning": "Thinking Process: …"},
				"finish_reason": "length",
			}},
		})
	}))
	defer srv.Close()

	_, err := openaiFor(srv.URL, "k", "m").Generate(context.Background(), Call{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "output cap") {
		t.Errorf("empty content with finish_reason=length should explain the cause, got %v", err)
	}
}

// A local server (Ollama, LM Studio) usually wants no auth at all — sending an
// empty Bearer header makes some of them 401.
func TestOpenAI_noKeySendsNoAuthHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Authorization"]; ok {
			t.Error("no key configured, but an Authorization header was sent")
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "ok"}}},
		})
	}))
	defer srv.Close()
	if _, err := openaiFor(srv.URL, "", "llama3.1").Generate(context.Background(), Call{Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
}
