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
	defaultOpenAIModel = "gpt-5.6-terra"
	defaultOpenAIURL   = "https://api.openai.com/v1"

	// Applied only to reasoning models. Override with $PGBOT_AI_REASONING_EFFORT
	// (none, low, medium, high, xhigh, max).
	defaultReasoningEffort = "xhigh"

	// A reasoning model's token cap covers hidden reasoning AND the visible
	// answer. At xhigh the reasoning alone can exceed the 8192 that suffices for
	// a non-reasoning model, leaving an empty response with finish_reason
	// "length" — so give reasoning models a floor well above it.
	reasoningTokenFloor = 32000
)

// OpenAIProvider speaks /chat/completions. That one wire format covers far more
// than OpenAI: OpenRouter, Groq, Together, Fireworks, DeepSeek, xAI, Mistral, and
// every local server (Ollama, vLLM, LM Studio) accept it, so pointing
// $PGBOT_AI_BASE_URL at any of them is the whole integration.
type OpenAIProvider struct {
	APIKey  string
	BaseURL string
	HTTP    *http.Client

	// ReasoningEffort is sent only to models that accept it; empty means the
	// provider's own default.
	ReasoningEffort string
}

func (p *OpenAIProvider) Name() string { return "openai" }

func (p *OpenAIProvider) LanguageModel(_ context.Context, modelID string) (LanguageModel, error) {
	if modelID == "" {
		modelID = defaultOpenAIModel
	}
	return &openaiModel{provider: p, model: modelID}, nil
}

type openaiModel struct {
	provider *OpenAIProvider
	model    string
}

func (m *openaiModel) Provider() string { return "openai" }
func (m *openaiModel) Model() string    { return m.model }
func (m *openaiModel) Endpoint() string { return m.provider.BaseURL }

// ---- wire types (only the fields we use) ----

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature *float64      `json:"temperature,omitempty"`
	// Exactly one of these is set. OpenAI's reasoning models reject max_tokens
	// outright ("use max_completion_tokens"), while every other compatible
	// server (Ollama, Groq, vLLM, LM Studio) understands only max_tokens — and
	// those are the reason this provider exists. reasoningModel picks.
	MaxTokens           *int64 `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int64 `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string `json:"reasoning_effort,omitempty"`
}

// reasoningModel reports whether a model id names an OpenAI-family reasoning
// model, which takes a different request shape from a plain chat model. Keyed on
// the id rather than the endpoint so it also fires for the same models reached
// through OpenRouter ("openai/gpt-5.6-terra") or Azure.
func reasoningModel(id string) bool {
	id = strings.ToLower(id)
	if i := strings.LastIndex(id, "/"); i >= 0 { // strip an "openai/" vendor prefix
		id = id[i+1:]
	}
	for _, p := range []string{"gpt-5", "o1", "o3", "o4"} {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	return false
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
}

// Generate sends one system + user turn and returns the model's text. Like every
// provider here it never retries — a failed explanation must not hang the CLI.
func (m *openaiModel) Generate(ctx context.Context, c Call) (*Response, error) {
	msgs := make([]chatMessage, 0, 2)
	if c.System != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: c.System})
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: c.Prompt})

	reqBody := chatRequest{Model: m.model, Messages: msgs}
	if reasoningModel(m.model) {
		// No temperature: the reasoning families reject sampling parameters, and
		// the low temperature we ask for elsewhere buys nothing on a model that
		// deliberates before answering.
		limit := int64(reasoningTokenFloor)
		if c.MaxOutputTokens != nil && *c.MaxOutputTokens > limit {
			limit = *c.MaxOutputTokens
		}
		reqBody.MaxCompletionTokens = &limit
		reqBody.ReasoningEffort = m.provider.ReasoningEffort
		if reqBody.ReasoningEffort == "" {
			reqBody.ReasoningEffort = defaultReasoningEffort
		}
	} else {
		reqBody.Temperature = c.Temperature
		reqBody.MaxTokens = c.MaxOutputTokens
	}

	buf, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.provider.BaseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.provider.APIKey != "" { // local servers often want no auth at all
		req.Header.Set("Authorization", "Bearer "+m.provider.APIKey) // header, never a query param
	}

	resp, err := m.provider.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", Host(m.provider.BaseURL), err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	// Errors first, decoded leniently: OpenAI nests an object under "error",
	// xAI returns a bare string there.
	var werr wireError
	if err := json.Unmarshal(body, &werr); err == nil {
		if msg := werr.message(); msg != "" {
			return nil, fmt.Errorf("%s error (HTTP %d): %s", Host(m.provider.BaseURL), resp.StatusCode, msg)
		}
	}

	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return nil, fmt.Errorf("%s returned unparseable response (HTTP %d)", Host(m.provider.BaseURL), resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s HTTP %d", Host(m.provider.BaseURL), resp.StatusCode)
	}
	if len(cr.Choices) == 0 {
		return nil, fmt.Errorf("%s returned no choices", Host(m.provider.BaseURL))
	}
	choice := cr.Choices[0]
	out := strings.TrimSpace(choice.Message.Content)
	if out == "" {
		// A reasoning model can burn the whole cap on hidden thinking and return
		// no visible text at all — common on small local models. Say what to do
		// about it rather than just reporting emptiness.
		if choice.FinishReason == "length" {
			return nil, fmt.Errorf("%s hit the output cap before writing an answer — the model spent it all on internal reasoning; lower $PGBOT_AI_REASONING_EFFORT, raise the cap, or pick a non-reasoning model", Host(m.provider.BaseURL))
		}
		return nil, fmt.Errorf("%s returned an empty explanation (finish: %s)", Host(m.provider.BaseURL), choice.FinishReason)
	}
	return &Response{Text: out, FinishReason: choice.FinishReason}, nil
}
