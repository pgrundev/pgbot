package ai

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Resolve builds the model to use from the environment. Keys come ONLY from the
// environment — never a flag — so they can't leak into shell history or the
// process list. That invariant is enforced here, once, for every provider.
//
// Precedence:
//
//	PGBOT_AI_PROVIDER          explicit: gemini | openai | anthropic | xai
//	                           otherwise auto-detected from whichever key is set,
//	                           OpenAI first to preserve existing behavior
//	PGBOT_AI_MODEL             model id (else the provider's default)
//	PGBOT_AI_BASE_URL          endpoint (else the provider's default)
//	PGBOT_AI_API_KEY           key (else the provider's conventional variable)
//	PGBOT_AI_REASONING_EFFORT  none|low|medium|high|xhigh|max, for reasoning models
//
// Existing PGBOT_OPENAI_* and PGBOT_GEMINI_* settings remain provider-scoped
// aliases for the general model and URL overrides.
func Resolve() (LanguageModel, error) {
	name := strings.ToLower(envOr("PGBOT_AI_PROVIDER", ""))
	if name == "" {
		var err error
		if name, err = detectProvider(); err != nil {
			return nil, err
		}
	}

	key := envOr("PGBOT_AI_API_KEY", "")
	base := envOr("PGBOT_AI_BASE_URL", "")
	model := envOr("PGBOT_AI_MODEL", "")
	// Generous because a local model on CPU can take minutes on a full report;
	// hosted providers answer in seconds and never come near this.
	httpc := &http.Client{Timeout: 3 * time.Minute}

	var p Provider
	switch name {
	case "gemini", "google":
		if key == "" {
			key = firstEnv("GEMINI_API_KEY", "GOOGLE_API_KEY")
		}
		if base == "" {
			base = envOr("PGBOT_GEMINI_URL", defaultGeminiURL)
		}
		if model == "" {
			model = envOr("PGBOT_GEMINI_MODEL", "")
		}
		p = &GeminiProvider{APIKey: key, BaseURL: trimURL(base), HTTP: httpc}

	case "anthropic", "claude":
		if key == "" {
			key = firstEnv("ANTHROPIC_API_KEY")
		}
		if base == "" {
			base = defaultAnthropicURL
		}
		p = &AnthropicProvider{APIKey: key, BaseURL: trimURL(base), HTTP: httpc}

	case "xai", "grok", "responses":
		// The Responses API, which xAI documents as its primary interface. Same
		// endpoint shape at OpenAI, so PGBOT_AI_PROVIDER=responses + an OpenAI key
		// and base URL works too.
		label := "xai"
		if key == "" {
			key = firstEnv("XAI_API_KEY", "GROK_API_KEY")
		}
		if key == "" && firstEnv("OPENAI_API_KEY") != "" {
			key, label = firstEnv("OPENAI_API_KEY"), "openai"
			if base == "" {
				base = envOr("PGBOT_OPENAI_URL", defaultOpenAIURL)
			}
			if model == "" {
				model = envOr("PGBOT_OPENAI_MODEL", defaultOpenAIModel)
			}
		}
		if base == "" {
			base = defaultXAIURL
		}
		p = &ResponsesProvider{
			APIKey:          key,
			BaseURL:         trimURL(base),
			HTTP:            httpc,
			Label:           label,
			ReasoningEffort: envOr("PGBOT_AI_REASONING_EFFORT", ""),
		}

	case "openai", "openrouter", "ollama", "openai-compatible":
		// One /chat/completions client serves them all; only the endpoint and the
		// conventional key variable differ.
		if key == "" {
			key = firstEnv("OPENAI_API_KEY", "OPENROUTER_API_KEY")
		}
		if base == "" {
			base = envOr("PGBOT_OPENAI_URL", "")
			if base == "" {
				base = defaultOpenAIURL
				if os.Getenv("OPENAI_API_KEY") == "" && os.Getenv("OPENROUTER_API_KEY") != "" {
					base = "https://openrouter.ai/api/v1"
				}
			}
		}
		if model == "" {
			model = envOr("PGBOT_OPENAI_MODEL", "")
		}
		p = &OpenAIProvider{
			APIKey:          key,
			BaseURL:         trimURL(base),
			HTTP:            httpc,
			ReasoningEffort: envOr("PGBOT_AI_REASONING_EFFORT", ""),
		}

	default:
		return nil, fmt.Errorf("unknown PGBOT_AI_PROVIDER %q (want gemini, openai, anthropic, or xai)", name)
	}

	// A local endpoint (Ollama, vLLM, LM Studio) usually has no key at all, and
	// nothing leaves the machine — don't demand one there.
	if key == "" && !Local(base) {
		return nil, fmt.Errorf("no API key for provider %q — set PGBOT_AI_API_KEY (or %s). The key is never read from a flag", name, keyVarsFor(name))
	}
	return p.LanguageModel(context.Background(), model)
}

// detectProvider picks a provider from whichever key is present. OpenAI remains
// first because that was the established precedence before more providers were
// added. PGBOT_AI_PROVIDER removes any ambiguity when several keys are present.
func detectProvider() (string, error) {
	switch {
	case firstEnv("OPENAI_API_KEY", "OPENROUTER_API_KEY") != "":
		return "openai", nil
	case firstEnv("GEMINI_API_KEY", "GOOGLE_API_KEY") != "":
		return "gemini", nil
	case firstEnv("ANTHROPIC_API_KEY") != "":
		return "anthropic", nil
	case firstEnv("XAI_API_KEY", "GROK_API_KEY") != "":
		return "xai", nil
	case envOr("PGBOT_AI_API_KEY", "") != "" || Local(envOr("PGBOT_AI_BASE_URL", "")):
		// A generic key or a local endpoint with no provider named: /chat/completions
		// is the safe assumption — it's what every local server speaks.
		return "openai", nil
	}
	return "", fmt.Errorf("no model configured for `pgbot explain` — set one of GEMINI_API_KEY, ANTHROPIC_API_KEY, OPENAI_API_KEY, OPENROUTER_API_KEY, or XAI_API_KEY " +
		"(or PGBOT_AI_PROVIDER + PGBOT_AI_API_KEY, or PGBOT_AI_BASE_URL for a local model). Keys are never read from a flag")
}

func keyVarsFor(name string) string {
	switch name {
	case "gemini", "google":
		return "GEMINI_API_KEY / GOOGLE_API_KEY"
	case "anthropic", "claude":
		return "ANTHROPIC_API_KEY"
	case "xai", "grok", "responses":
		return "XAI_API_KEY / GROK_API_KEY"
	default:
		return "OPENAI_API_KEY / OPENROUTER_API_KEY"
	}
}

func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

func trimURL(s string) string { return strings.TrimRight(s, "/") }
