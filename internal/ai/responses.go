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
	defaultXAIModel = "grok-4.6"
	defaultXAIURL   = "https://api.x.ai/v1"
)

// ResponsesProvider speaks the Responses API (POST /responses) — the newer
// surface both xAI and OpenAI prefer over /chat/completions. pgbot uses it for
// xAI, where it is the documented primary interface.
//
// It is deliberately NOT the default for the OpenAI-compatible world: only
// OpenAI and xAI implement /responses, while Ollama, vLLM, LM Studio, Groq,
// Together, DeepSeek and Mistral implement only /chat/completions. This provider
// is additive — OpenAIProvider stays the compatibility path.
type ResponsesProvider struct {
	APIKey  string
	BaseURL string
	HTTP    *http.Client

	// Label is the provider name shown in the consent prompt and AI banner
	// ("xai", "openai") — the endpoint is shared, the vendor is not.
	Label string

	// ReasoningEffort is sent as reasoning.effort when set. Left empty by default
	// because the accepted vocabulary differs per vendor; the model's own default
	// is the safe choice.
	ReasoningEffort string
}

func (p *ResponsesProvider) Name() string {
	if p.Label != "" {
		return p.Label
	}
	return "responses"
}

func (p *ResponsesProvider) LanguageModel(_ context.Context, modelID string) (LanguageModel, error) {
	if modelID == "" {
		modelID = defaultXAIModel
	}
	return &responsesModel{provider: p, model: modelID}, nil
}

type responsesModel struct {
	provider *ResponsesProvider
	model    string
}

func (m *responsesModel) Provider() string { return m.provider.Name() }
func (m *responsesModel) Model() string    { return m.model }
func (m *responsesModel) Endpoint() string { return m.provider.BaseURL }

// ---- wire types (only the fields we use) ----

type responsesRequest struct {
	Model        string `json:"model"`
	Instructions string `json:"instructions,omitempty"` // the system turn
	Input        string `json:"input"`                  // the user turn
	// Store is explicitly false. The Responses API defaults it to TRUE, which
	// retains the request server-side for later retrieval — a quiet downgrade of
	// the disclosure `pgbot explain` asks the user to consent to. We send the
	// findings once and keep nothing on the vendor's side.
	Store           bool          `json:"store"`
	MaxOutputTokens *int64        `json:"max_output_tokens,omitempty"`
	Temperature     *float64      `json:"temperature,omitempty"`
	Reasoning       *reasoningCfg `json:"reasoning,omitempty"`
}

type reasoningCfg struct {
	Effort string `json:"effort,omitempty"`
}

type responsesResponse struct {
	Status string `json:"status"` // "completed" | "incomplete" | …
	Output []struct {
		Type    string `json:"type"` // "reasoning" | "message" | …
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"` // "output_text" | …
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

// Generate sends one instructions + input turn and returns the model's text. No
// retries — a failed explanation must not hang the CLI.
func (m *responsesModel) Generate(ctx context.Context, c Call) (*Response, error) {
	reqBody := responsesRequest{
		Model:           m.model,
		Instructions:    c.System,
		Input:           c.Prompt,
		Store:           false,
		MaxOutputTokens: c.MaxOutputTokens,
		Temperature:     c.Temperature,
	}
	if e := m.provider.ReasoningEffort; e != "" {
		reqBody.Reasoning = &reasoningCfg{Effort: e}
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.provider.BaseURL+"/responses", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.provider.APIKey) // header, never a query param

	resp, err := m.provider.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", m.provider.Name(), err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	// Errors first: this API returns them as a top-level field, and xAI's are a
	// bare string where OpenAI's are an object.
	var werr wireError
	if err := json.Unmarshal(body, &werr); err == nil {
		if msg := werr.message(); msg != "" {
			return nil, fmt.Errorf("%s error (HTTP %d): %s", m.provider.Name(), resp.StatusCode, msg)
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s HTTP %d", m.provider.Name(), resp.StatusCode)
	}

	var rr responsesResponse
	if err := json.Unmarshal(body, &rr); err != nil {
		return nil, fmt.Errorf("%s returned unparseable response (HTTP %d)", m.provider.Name(), resp.StatusCode)
	}

	// The answer is one entry in `output` — the rest is reasoning, which we never
	// surface. There is no `output_text` on the wire; that is an SDK convenience.
	var sb strings.Builder
	for _, o := range rr.Output {
		if o.Type != "message" {
			continue
		}
		for _, ct := range o.Content {
			if ct.Type == "output_text" {
				sb.WriteString(ct.Text)
			}
		}
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		if rr.IncompleteDetails != nil && rr.IncompleteDetails.Reason == "max_output_tokens" {
			return nil, fmt.Errorf("%s hit the output cap before writing an answer — the model spent it all on internal reasoning; lower $PGBOT_AI_REASONING_EFFORT or raise the cap", m.provider.Name())
		}
		return nil, fmt.Errorf("%s returned an empty explanation (status: %s)", m.provider.Name(), rr.Status)
	}
	return &Response{Text: out, FinishReason: rr.Status}, nil
}
