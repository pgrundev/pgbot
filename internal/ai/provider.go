// Package ai is pgbot's OPTIONAL explanation layer. It never produces findings —
// those are computed deterministically in package findings. It only takes an
// already-computed, PII-free Context and asks a model to explain and prioritize
// it in plain language. Everything it emits is labeled as model-generated.
//
// The model is yours to choose: Gemini, Anthropic, OpenAI, or any OpenAI-compatible
// endpoint (OpenRouter, Groq, Together, DeepSeek, xAI, Mistral, Ollama, vLLM,
// LM Studio). Each provider is a few hundred lines of net/http so pgbot keeps its
// single-static-binary, minimal-dependency promise — no vendor SDKs.
package ai

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
)

// Provider is a named source of language models.
//
// This mirrors charmbracelet/fantasy's Provider/LanguageModel pair on purpose, so
// a fantasy-backed implementation could drop in later — but we implement it over
// net/http instead of depending on fantasy, which pulls the real vendor SDKs
// (anthropic-sdk-go, openai-go, google.golang.org/genai, aws-sdk-go-v2) and takes
// the binary from 23 MB to ~65 MB for one non-streaming POST. The interface is
// narrowed to the single call pgbot makes: one system turn, one user turn, no
// tools, no streaming.
type Provider interface {
	Name() string
	LanguageModel(ctx context.Context, modelID string) (LanguageModel, error)
}

// LanguageModel is one model at one endpoint, ready to answer a single turn.
type LanguageModel interface {
	Generate(ctx context.Context, c Call) (*Response, error)
	Provider() string // "gemini" | "openai" | "anthropic"
	Model() string    // resolved model id — shown in the AI banner
	Endpoint() string // base URL we POST to — powers the consent prompt
}

// Call is one system+user turn. Temperature and MaxOutputTokens are hints: a
// provider omits either when its API rejects it (Anthropic's current models
// return 400 for `temperature`), which is why both are pointers.
type Call struct {
	System          string
	Prompt          string
	Temperature     *float64
	MaxOutputTokens *int64
}

// Response is the model's text plus why it stopped.
type Response struct {
	Text         string
	FinishReason string
}

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

// wireError decodes the incompatible error payloads these APIs actually return:
// OpenAI and Anthropic nest an object under "error", xAI puts a bare string
// there. Modelling it as a struct silently fails on xAI and loses the message —
// which is the one thing the user needs when a key or model id is wrong.
type wireError struct {
	Error json.RawMessage `json:"error"`
}

func (w wireError) message() string {
	if len(w.Error) == 0 || string(w.Error) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(w.Error, &s) == nil { // {"error": "Incorrect API key provided."}
		return s
	}
	var o struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(w.Error, &o) == nil && o.Message != "" { // {"error": {"message": …}}
		return o.Message
	}
	return strings.TrimSpace(string(w.Error))
}

// Local reports whether a base URL points at this machine. `pgbot explain` is the
// only command that can send data off the box — when it can't (Ollama, vLLM, LM
// Studio on localhost), the consent prompt should say so rather than warn about a
// disclosure that isn't happening.
func Local(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	return false
}

// Host is the display form of an endpoint for the consent prompt — host[:port],
// never the full path, and never anything that could carry a key.
func Host(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return endpoint
	}
	return u.Host
}
