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
	// A moving alias, on purpose: pinned versions get retired (e.g. gemini-2.5-flash
	// now 404s for new projects), and an explanation feature doesn't need a frozen
	// model. Override with $PGBOT_AI_MODEL to pin one.
	defaultGeminiModel = "gemini-flash-latest"
	defaultGeminiURL   = "https://generativelanguage.googleapis.com/v1beta"
)

// GeminiProvider talks to Google's Generative Language REST API. Both AI-Studio
// "auth" keys (AQ.…) and legacy standard keys (AIza…) work — they travel in the
// same header.
type GeminiProvider struct {
	APIKey  string
	BaseURL string
	HTTP    *http.Client
}

func (p *GeminiProvider) Name() string { return "gemini" }

func (p *GeminiProvider) LanguageModel(_ context.Context, modelID string) (LanguageModel, error) {
	if modelID == "" {
		modelID = defaultGeminiModel
	}
	return &geminiModel{provider: p, model: modelID}, nil
}

type geminiModel struct {
	provider *GeminiProvider
	model    string
}

func (m *geminiModel) Provider() string { return "gemini" }
func (m *geminiModel) Model() string    { return m.model }
func (m *geminiModel) Endpoint() string { return m.provider.BaseURL }

// ---- wire types (only the fields we use) ----

type genRequest struct {
	SystemInstruction *content         `json:"systemInstruction,omitempty"`
	Contents          []content        `json:"contents"`
	GenerationConfig  generationConfig `json:"generationConfig"`
}

type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

type part struct {
	Text string `json:"text"`
}

type generationConfig struct {
	Temperature     float64 `json:"temperature"`
	MaxOutputTokens int     `json:"maxOutputTokens"`
}

type genResponse struct {
	Candidates []struct {
		Content      content `json:"content"`
		FinishReason string  `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	Error *apiError `json:"error"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// Generate sends one system + user turn and returns the model's text. It never
// retries with backoff loops — a failed explanation must not hang the CLI; the
// caller degrades to the deterministic report alone.
func (m *geminiModel) Generate(ctx context.Context, c Call) (*Response, error) {
	cfg := generationConfig{Temperature: 0.2, MaxOutputTokens: 8192}
	if c.Temperature != nil {
		cfg.Temperature = *c.Temperature
	}
	if c.MaxOutputTokens != nil {
		cfg.MaxOutputTokens = int(*c.MaxOutputTokens)
	}
	reqBody := genRequest{
		Contents: []content{{Role: "user", Parts: []part{{Text: c.Prompt}}}},
		// Headroom for the answer AND for the internal "thinking" that current
		// Gemini models spend before the visible text — a tight cap truncates the
		// explanation mid-sentence.
		GenerationConfig: cfg,
	}
	if c.System != "" {
		reqBody.SystemInstruction = &content{Parts: []part{{Text: c.System}}}
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/models/%s:generateContent", m.provider.BaseURL, m.model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", m.provider.APIKey) // header, not ?key= — keeps the key out of any URL log

	resp, err := m.provider.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling Gemini: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var gr genResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return nil, fmt.Errorf("gemini returned unparseable response (HTTP %d)", resp.StatusCode)
	}
	if gr.Error != nil {
		return nil, fmt.Errorf("gemini error (%d %s): %s", gr.Error.Code, gr.Error.Status, gr.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gemini HTTP %d", resp.StatusCode)
	}
	if gr.PromptFeedback != nil && gr.PromptFeedback.BlockReason != "" {
		return nil, fmt.Errorf("gemini blocked the prompt: %s", gr.PromptFeedback.BlockReason)
	}
	if len(gr.Candidates) == 0 {
		return nil, fmt.Errorf("gemini returned no candidates")
	}
	var sb strings.Builder
	for _, p := range gr.Candidates[0].Content.Parts {
		sb.WriteString(p.Text)
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		return nil, fmt.Errorf("gemini returned an empty explanation (finish: %s)", gr.Candidates[0].FinishReason)
	}
	return &Response{Text: out, FinishReason: gr.Candidates[0].FinishReason}, nil
}
