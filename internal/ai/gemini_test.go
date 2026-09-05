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

// geminiFor builds a model pointed at a test server, bypassing the environment.
func geminiFor(url, key, model string) LanguageModel {
	p := &GeminiProvider{APIKey: key, BaseURL: url, HTTP: http.DefaultClient}
	m, _ := p.LanguageModel(context.Background(), model)
	return m
}

func TestGemini_success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-goog-api-key"); got != "k123" {
			t.Errorf("API key must travel in the x-goog-api-key header, got %q", got)
		}
		if !strings.Contains(r.URL.Path, "/models/m1:generateContent") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		// The key must never appear in the URL (query or path).
		if strings.Contains(r.URL.String(), "k123") {
			t.Error("API key leaked into the URL")
		}
		body, _ := io.ReadAll(r.Body)
		var req genRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("bad request body: %v", err)
		}
		if req.SystemInstruction == nil || len(req.Contents) != 1 {
			t.Errorf("request shape wrong: %+v", req)
		}
		json.NewEncoder(w).Encode(genResponse{Candidates: []struct {
			Content      content `json:"content"`
			FinishReason string  `json:"finishReason"`
		}{{Content: content{Parts: []part{{Text: "Looks healthy."}}}, FinishReason: "STOP"}}})
	}))
	defer srv.Close()

	out, err := geminiFor(srv.URL, "k123", "m1").Generate(context.Background(), Call{System: "be terse", Prompt: "the report"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "Looks healthy." {
		t.Errorf("unexpected output %q", out.Text)
	}
}

func TestGemini_apiError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(genResponse{Error: &apiError{Code: 400, Status: "INVALID_ARGUMENT", Message: "API key not valid"}})
	}))
	defer srv.Close()

	_, err := geminiFor(srv.URL, "bad", "m1").Generate(context.Background(), Call{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "API key not valid") {
		t.Errorf("expected a surfaced API error, got %v", err)
	}
}

func TestGemini_noCandidates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(genResponse{}) // 200, no candidates
	}))
	defer srv.Close()
	if _, err := geminiFor(srv.URL, "k", "m").Generate(context.Background(), Call{Prompt: "x"}); err == nil {
		t.Error("no candidates should be an error, not an empty success")
	}
}

func TestGemini_defaultModel(t *testing.T) {
	m := geminiFor("https://example.test", "k", "")
	if m.Model() != defaultGeminiModel {
		t.Errorf("empty model id should fall back to the default, got %q", m.Model())
	}
	if m.Provider() != "gemini" {
		t.Errorf("provider name wrong: %q", m.Provider())
	}
}
