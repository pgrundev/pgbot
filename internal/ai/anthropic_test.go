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

func anthropicFor(url, key, model string) LanguageModel {
	p := &AnthropicProvider{APIKey: key, BaseURL: url, HTTP: http.DefaultClient}
	m, _ := p.LanguageModel(context.Background(), model)
	return m
}

func TestAnthropic_success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "k123" {
			t.Errorf("key must travel in the x-api-key header, got %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != anthropicVersion {
			t.Errorf("anthropic-version header missing or wrong: %q", got)
		}
		if strings.Contains(r.URL.String(), "k123") {
			t.Error("API key leaked into the URL")
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)

		// temperature is REMOVED on current Anthropic models — sending it is a 400,
		// so it must never appear even though Call carries one.
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("bad request body: %v", err)
		}
		if _, ok := raw["temperature"]; ok {
			t.Error("temperature must not be sent to Anthropic — current models reject it")
		}
		if raw["max_tokens"] == nil {
			t.Error("max_tokens is required by the Messages API")
		}
		if raw["system"] != "be terse" {
			t.Errorf("system prompt not sent as a top-level field: %v", raw["system"])
		}

		json.NewEncoder(w).Encode(map[string]any{
			"content":     []map[string]string{{"type": "text", "text": "Looks healthy."}},
			"stop_reason": "end_turn",
		})
	}))
	defer srv.Close()

	out, err := anthropicFor(srv.URL, "k123", "m1").Generate(context.Background(), Call{
		System: "be terse", Prompt: "the report", Temperature: f64(0.2),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "Looks healthy." {
		t.Errorf("unexpected output %q", out.Text)
	}
}

func TestAnthropic_apiError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"type":  "error",
			"error": map[string]string{"type": "authentication_error", "message": "invalid x-api-key"},
		})
	}))
	defer srv.Close()

	_, err := anthropicFor(srv.URL, "bad", "m1").Generate(context.Background(), Call{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "invalid x-api-key") {
		t.Errorf("expected a surfaced API error, got %v", err)
	}
}

// A refusal is a successful HTTP 200 with empty content — it must not read as an
// unexplained blank answer.
func TestAnthropic_refusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"content":      []any{},
			"stop_reason":  "refusal",
			"stop_details": map[string]string{"type": "refusal", "category": "cyber"},
		})
	}))
	defer srv.Close()

	_, err := anthropicFor(srv.URL, "k", "m").Generate(context.Background(), Call{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "declined") {
		t.Errorf("a refusal must surface as an error naming the reason, got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "cyber") {
		t.Errorf("refusal category should be reported, got %v", err)
	}
}

func TestAnthropic_emptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"content": []any{}, "stop_reason": "max_tokens"})
	}))
	defer srv.Close()
	if _, err := anthropicFor(srv.URL, "k", "m").Generate(context.Background(), Call{Prompt: "x"}); err == nil {
		t.Error("empty content should be an error, not an empty success")
	}
}
