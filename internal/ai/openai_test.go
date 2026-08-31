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

func TestOpenAIGenerate_success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("API key must travel in the Authorization: Bearer header, got %q", got)
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if strings.Contains(r.URL.String(), "sk-test") {
			t.Error("API key leaked into the URL")
		}
		body, _ := io.ReadAll(r.Body)
		var req chatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("bad request body: %v", err)
		}
		if req.Model != "gpt-test" || len(req.Messages) != 2 {
			t.Errorf("request shape wrong: %+v", req)
		}
		if req.Temperature == nil || *req.Temperature != 0.2 {
			t.Errorf("temperature = %v, want 0.2", req.Temperature)
		}
		if req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
			t.Errorf("expected system then user message, got %+v", req.Messages)
		}
		var resp chatResponse
		resp.Choices = append(resp.Choices, struct {
			Message      chatMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		}{Message: chatMessage{Role: "assistant", Content: "Looks healthy."}, FinishReason: "stop"})
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &OpenAIClient{APIKey: "sk-test", Model: "gpt-test", BaseURL: srv.URL, HTTP: srv.Client()}
	out, err := c.Generate(context.Background(), "be terse", "the report")
	if err != nil {
		t.Fatal(err)
	}
	if out != "Looks healthy." {
		t.Errorf("unexpected output %q", out)
	}
	if c.Vendor() != "OpenAI" || c.ModelName() != "gpt-test" {
		t.Errorf("Vendor/ModelName wrong: %q %q", c.Vendor(), c.ModelName())
	}
}

func TestOpenAIGenerate_GPT56OmitsTemperature(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("bad request body: %v", err)
		}
		if _, ok := req["temperature"]; ok {
			t.Errorf("GPT-5.6 request must omit temperature: %s", req["temperature"])
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Looks healthy."},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	c := &OpenAIClient{APIKey: "sk-test", Model: "gpt-5.6-sol", BaseURL: srv.URL, HTTP: srv.Client()}
	if _, err := c.Generate(context.Background(), "be terse", "the report"); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAIGenerate_RetriesUnsupportedTemperature(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var req map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("bad request body: %v", err)
		}
		_, hasTemperature := req["temperature"]
		if requests == 1 {
			if !hasTemperature {
				t.Error("first request should use the configured temperature")
			}
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"Unsupported value: 'temperature' does not support 0.2 with this model. Only the default (1) value is supported.","type":"invalid_request_error"}}`)
			return
		}
		if hasTemperature {
			t.Error("compatibility retry must omit temperature")
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Recovered."},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	c := &OpenAIClient{APIKey: "sk-test", Model: "future-model-alias", BaseURL: srv.URL, HTTP: srv.Client()}
	out, err := c.Generate(context.Background(), "", "the report")
	if err != nil {
		t.Fatal(err)
	}
	if out != "Recovered." || requests != 2 {
		t.Errorf("got output %q after %d requests", out, requests)
	}
}

func TestOpenAIGenerate_apiError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()
	c := &OpenAIClient{APIKey: "bad", Model: "m", BaseURL: srv.URL, HTTP: srv.Client()}
	_, err := c.Generate(context.Background(), "", "x")
	if err == nil || !strings.Contains(err.Error(), "Incorrect API key") {
		t.Errorf("expected a surfaced API error, got %v", err)
	}
}

func TestNewOpenAIFromEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	if _, err := NewOpenAIFromEnv(); err == nil {
		t.Error("missing key should error")
	}
	t.Setenv("OPENAI_API_KEY", "sk-abc")
	c, err := NewOpenAIFromEnv()
	if err != nil || c.APIKey != "sk-abc" {
		t.Fatalf("unexpected: %v %+v", err, c)
	}
	if c.Model != defaultOpenAIModel {
		t.Errorf("default model = %q, want %q", c.Model, defaultOpenAIModel)
	}
	t.Setenv("PGBOT_OPENAI_MODEL", "gpt-4.1")
	c, _ = NewOpenAIFromEnv()
	if c.Model != "gpt-4.1" {
		t.Errorf("model override not honored: %q", c.Model)
	}
}
