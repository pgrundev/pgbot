package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	// The most capable current model. `pgbot explain` is a one-shot call on a
	// small payload, so this is cheap in absolute terms — but set
	// $PGBOT_AI_MODEL=claude-haiku-4-5 if you run it in a tight loop.
	defaultAnthropicModel = "claude-opus-5"
	defaultAnthropicURL   = "https://api.anthropic.com"
	anthropicVersion      = "2023-06-01"
)

// AnthropicProvider talks to the Messages API.
type AnthropicProvider struct {
	APIKey  string
	BaseURL string
	HTTP    *http.Client
}

func (p *AnthropicProvider) Name() string { return "anthropic" }

func (p *AnthropicProvider) LanguageModel(_ context.Context, modelID string) (LanguageModel, error) {
	if modelID == "" {
		modelID = defaultAnthropicModel
	}
	return &anthropicModel{provider: p, model: modelID}, nil
}

type anthropicModel struct {
	provider *AnthropicProvider
	model    string
}

func (m *anthropicModel) Provider() string { return "anthropic" }
func (m *anthropicModel) Model() string    { return m.model }
func (m *anthropicModel) Endpoint() string { return m.provider.BaseURL }

// ---- wire types (only the fields we use) ----

type messagesRequest struct {
	Model  string `json:"model"`
	System string `json:"system,omitempty"`
	// Required by the API, and it caps thinking + visible text together: current
	// models think before they answer, so a tight value truncates the explanation
	// mid-sentence. Same headroom, same reason, as the Gemini path.
	MaxTokens int                `json:"max_tokens"`
	Messages  []anthropicMessage `json:"messages"`
	// Deliberately no `temperature`: it is REMOVED on current models (Opus 5,
	// Sonnet 5, Opus 4.7+) and sending it returns a 400. Call.Temperature is a
	// hint, and this provider ignores it.
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason  string `json:"stop_reason"`
	StopDetails *struct {
		Category string `json:"category"`
	} `json:"stop_details"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Generate sends one system + user turn and returns the model's text. No retries
// — a failed explanation must not hang the CLI.
func (m *anthropicModel) Generate(ctx context.Context, c Call) (*Response, error) {
	maxTokens := 8192
	if c.MaxOutputTokens != nil {
		maxTokens = int(*c.MaxOutputTokens)
	}
	buf, err := json.Marshal(messagesRequest{
		Model:     m.model,
		System:    c.System,
		MaxTokens: maxTokens,
		Messages:  []anthropicMessage{{Role: "user", Content: c.Prompt}},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.provider.BaseURL+"/v1/messages", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("x-api-key", m.provider.APIKey) // header, never a query param

	resp, err := m.provider.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling Anthropic: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var mr messagesResponse
	if err := json.Unmarshal(body, &mr); err != nil {
		return nil, fmt.Errorf("anthropic returned unparseable response (HTTP %d)", resp.StatusCode)
	}
	if mr.Error != nil {
		return nil, fmt.Errorf("anthropic error (%s): %s", mr.Error.Type, mr.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anthropic HTTP %d", resp.StatusCode)
	}
	// A refusal is a successful HTTP 200 with empty content — check it before
	// reading the blocks, or it looks like an unexplained empty answer.
	if mr.StopReason == "refusal" {
		reason := "safety"
		if mr.StopDetails != nil && mr.StopDetails.Category != "" {
			reason = mr.StopDetails.Category
		}
		return nil, fmt.Errorf("anthropic declined the prompt (%s)", reason)
	}
	var sb strings.Builder
	for _, blk := range mr.Content {
		if blk.Type == "text" {
			sb.WriteString(blk.Text)
		}
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		return nil, fmt.Errorf("anthropic returned an empty explanation (finish: %s)", mr.StopReason)
	}
	return &Response{Text: out, FinishReason: mr.StopReason}, nil
}
