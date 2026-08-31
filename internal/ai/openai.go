package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// A small, cheap chat model is plenty for an explanation task. Override with
	// $PGBOT_OPENAI_MODEL to pin a bigger or newer one.
	defaultOpenAIModel = "gpt-4o-mini"
	defaultOpenAIURL   = "https://api.openai.com/v1"
)

// OpenAIClient is a minimal Chat Completions client. Because it speaks the plain
// OpenAI wire format, it also works against any OpenAI-compatible endpoint via
// $PGBOT_OPENAI_URL (Azure OpenAI, OpenRouter, a local server, …).
type OpenAIClient struct {
	APIKey  string
	Model   string
	BaseURL string
	HTTP    *http.Client
}

// NewOpenAIFromEnv builds a client from $OPENAI_API_KEY. $PGBOT_OPENAI_MODEL and
// $PGBOT_OPENAI_URL override the defaults. The key is read from the environment
// only — never a flag — so it stays out of shell history and the process list.
func NewOpenAIFromEnv() (*OpenAIClient, error) {
	key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if key == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY is not set — export it to use `pgbot explain`/`ask` with OpenAI (the key is never read from a flag)")
	}
	return &OpenAIClient{
		APIKey:  key,
		Model:   envOr("PGBOT_OPENAI_MODEL", defaultOpenAIModel),
		BaseURL: strings.TrimRight(envOr("PGBOT_OPENAI_URL", defaultOpenAIURL), "/"),
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func (c *OpenAIClient) ModelName() string { return c.Model }
func (c *OpenAIClient) Vendor() string    { return "OpenAI" }

// ---- wire types (only the fields we use) ----

type chatRequest struct {
	Model               string        `json:"model"`
	Messages            []chatMessage `json:"messages"`
	Temperature         *float64      `json:"temperature,omitempty"`
	MaxCompletionTokens int           `json:"max_completion_tokens,omitempty"`
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
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// Generate sends one system + user turn and returns the model's text. It makes
// one compatibility retry without temperature when an OpenAI-compatible API
// rejects a custom value; otherwise the caller degrades to the deterministic
// report alone on failure.
func (c *OpenAIClient) Generate(ctx context.Context, system, user string) (string, error) {
	msgs := make([]chatMessage, 0, 2)
	if system != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: system})
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: user})

	payload := chatRequest{
		Model:               c.Model,
		Messages:            msgs,
		Temperature:         openAITemperature(c.Model),
		MaxCompletionTokens: 8192,
	}
	out, retryWithoutTemperature, err := c.generate(ctx, payload)
	if err != nil && payload.Temperature != nil && retryWithoutTemperature {
		payload.Temperature = nil
		out, _, err = c.generate(ctx, payload)
	}
	return out, err
}

// GPT-5.6 models only accept the default temperature, so omitting the field is
// both valid and avoids a guaranteed rejected request. Other models retain the
// low temperature used for deterministic-ish diagnostic explanations.
func openAITemperature(model string) *float64 {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "gpt-5.6") {
		return nil
	}
	temperature := 0.2
	return &temperature
}

func (c *OpenAIClient) generate(ctx context.Context, payload chatRequest) (string, bool, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return "", false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey) // header, never a URL param

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("calling OpenAI: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", false, fmt.Errorf("openai returned unparseable response (HTTP %d)", resp.StatusCode)
	}
	if cr.Error != nil {
		return "", unsupportedTemperatureMessage(cr.Error.Message), fmt.Errorf("openai error: %s", cr.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("openai HTTP %d", resp.StatusCode)
	}
	if len(cr.Choices) == 0 {
		return "", false, fmt.Errorf("openai returned no choices")
	}
	out := strings.TrimSpace(cr.Choices[0].Message.Content)
	if out == "" {
		return "", false, fmt.Errorf("openai returned an empty explanation (finish: %s)", cr.Choices[0].FinishReason)
	}
	return out, false, nil
}

func unsupportedTemperatureMessage(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "temperature") &&
		(strings.Contains(message, "unsupported") ||
			strings.Contains(message, "does not support") ||
			strings.Contains(message, "only the default"))
}
