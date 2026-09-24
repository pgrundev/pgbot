package ai

import (
	"strings"
	"testing"
)

// clearEnv unsets every variable Resolve reads, so each case starts from nothing
// and can't be perturbed by the developer's own shell.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"PGBOT_AI_PROVIDER", "PGBOT_AI_MODEL", "PGBOT_AI_BASE_URL", "PGBOT_AI_API_KEY",
		"PGBOT_AI_REASONING_EFFORT", "GEMINI_API_KEY", "GOOGLE_API_KEY", "ANTHROPIC_API_KEY",
		"OPENAI_API_KEY", "OPENROUTER_API_KEY", "REQUESTY_API_KEY", "XAI_API_KEY", "GROK_API_KEY",
		"AWS_BEARER_TOKEN_BEDROCK", "AWS_REGION", "AWS_DEFAULT_REGION",
		"PGBOT_GEMINI_MODEL", "PGBOT_GEMINI_URL", "PGBOT_OPENAI_MODEL", "PGBOT_OPENAI_URL",
	} {
		t.Setenv(k, "")
	}
}

func TestResolve_noConfiguration(t *testing.T) {
	clearEnv(t)
	_, err := Resolve()
	if err == nil {
		t.Fatal("no key anywhere must be an error")
	}
	// The error has to name every accepted variable, not just Gemini's.
	for _, want := range []string{"GEMINI_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "never read from a flag"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestResolve_autodetect(t *testing.T) {
	cases := []struct {
		name         string
		env          map[string]string
		wantProvider string
		wantEndpoint string
	}{
		{"gemini", map[string]string{"GEMINI_API_KEY": "k"}, "gemini", defaultGeminiURL},
		{"google fallback", map[string]string{"GOOGLE_API_KEY": "k"}, "gemini", defaultGeminiURL},
		{"anthropic", map[string]string{"ANTHROPIC_API_KEY": "k"}, "anthropic", defaultAnthropicURL},
		{"openai", map[string]string{"OPENAI_API_KEY": "k"}, "openai", defaultOpenAIURL},
		{"openrouter", map[string]string{"OPENROUTER_API_KEY": "k"}, "openai", "https://openrouter.ai/api/v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			m, err := Resolve()
			if err != nil {
				t.Fatal(err)
			}
			if m.Provider() != tc.wantProvider {
				t.Errorf("provider = %q, want %q", m.Provider(), tc.wantProvider)
			}
			if m.Endpoint() != tc.wantEndpoint {
				t.Errorf("endpoint = %q, want %q", m.Endpoint(), tc.wantEndpoint)
			}
		})
	}
}

// OpenAI already won auto-detection before Anthropic and xAI were added.
func TestResolve_openAIWinsAutodetect(t *testing.T) {
	clearEnv(t)
	t.Setenv("GEMINI_API_KEY", "g")
	t.Setenv("ANTHROPIC_API_KEY", "a")
	t.Setenv("OPENAI_API_KEY", "o")
	m, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if m.Provider() != "openai" {
		t.Errorf("openai must win auto-detection, got %q", m.Provider())
	}
}

func TestResolve_explicitProviderBeatsAutodetect(t *testing.T) {
	clearEnv(t)
	t.Setenv("GEMINI_API_KEY", "g")
	t.Setenv("ANTHROPIC_API_KEY", "a")
	t.Setenv("PGBOT_AI_PROVIDER", "anthropic")
	m, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if m.Provider() != "anthropic" {
		t.Errorf("PGBOT_AI_PROVIDER must win, got %q", m.Provider())
	}
	if m.Model() != defaultAnthropicModel {
		t.Errorf("model = %q, want the anthropic default", m.Model())
	}
}

func TestResolve_modelAndURLOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("PGBOT_AI_PROVIDER", "gemini")
	t.Setenv("GEMINI_API_KEY", "k")
	t.Setenv("PGBOT_AI_MODEL", "gemini-3-pro")
	t.Setenv("PGBOT_AI_BASE_URL", "https://proxy.example/v1/")
	m, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if m.Model() != "gemini-3-pro" {
		t.Errorf("model = %q", m.Model())
	}
	if m.Endpoint() != "https://proxy.example/v1" {
		t.Errorf("trailing slash should be trimmed, got %q", m.Endpoint())
	}
}

// The pre-BYOK variables keep working.
func TestResolve_legacyGeminiVars(t *testing.T) {
	clearEnv(t)
	t.Setenv("GEMINI_API_KEY", "k")
	t.Setenv("PGBOT_GEMINI_MODEL", "gemini-3-pro")
	t.Setenv("PGBOT_GEMINI_URL", "https://legacy.example/v1beta")
	m, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if m.Model() != "gemini-3-pro" {
		t.Errorf("PGBOT_GEMINI_MODEL not honored: %q", m.Model())
	}
	if m.Endpoint() != "https://legacy.example/v1beta" {
		t.Errorf("PGBOT_GEMINI_URL not honored: %q", m.Endpoint())
	}
}

func TestResolve_legacyOpenAIVars(t *testing.T) {
	clearEnv(t)
	t.Setenv("OPENAI_API_KEY", "k")
	t.Setenv("PGBOT_OPENAI_MODEL", "gpt-4.1")
	t.Setenv("PGBOT_OPENAI_URL", "https://legacy-openai.example/v1/")
	m, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if m.Model() != "gpt-4.1" {
		t.Errorf("PGBOT_OPENAI_MODEL not honored: %q", m.Model())
	}
	if m.Endpoint() != "https://legacy-openai.example/v1" {
		t.Errorf("PGBOT_OPENAI_URL not honored: %q", m.Endpoint())
	}
}

// A local model needs no key and discloses nothing; a remote one still must have
// a key before we'd send findings to it.
func TestResolve_localEndpointNeedsNoKey(t *testing.T) {
	clearEnv(t)
	t.Setenv("PGBOT_AI_BASE_URL", "http://localhost:11434/v1")
	t.Setenv("PGBOT_AI_MODEL", "llama3.1")
	m, err := Resolve()
	if err != nil {
		t.Fatalf("a local endpoint should not require a key: %v", err)
	}
	if m.Provider() != "openai" {
		t.Errorf("a bare local endpoint should default to the OpenAI-compatible client, got %q", m.Provider())
	}
	if !Local(m.Endpoint()) {
		t.Errorf("%q should be recognized as local", m.Endpoint())
	}
}

func TestResolve_remoteEndpointRequiresKey(t *testing.T) {
	clearEnv(t)
	t.Setenv("PGBOT_AI_PROVIDER", "openai")
	t.Setenv("PGBOT_AI_BASE_URL", "https://api.example.com/v1")
	if _, err := Resolve(); err == nil {
		t.Error("a remote endpoint with no key must be an error")
	}
}

func TestResolve_unknownProvider(t *testing.T) {
	clearEnv(t)
	t.Setenv("PGBOT_AI_PROVIDER", "unknown-provider")
	t.Setenv("PGBOT_AI_API_KEY", "k")
	if _, err := Resolve(); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("unknown provider should be rejected clearly, got %v", err)
	}
}

func TestLocalAndHost(t *testing.T) {
	for _, u := range []string{"http://localhost:11434/v1", "http://127.0.0.1:8000/v1", "http://[::1]:1234/v1"} {
		if !Local(u) {
			t.Errorf("%q should be local", u)
		}
	}
	for _, u := range []string{defaultGeminiURL, defaultAnthropicURL, "https://openrouter.ai/api/v1"} {
		if Local(u) {
			t.Errorf("%q should not be local", u)
		}
	}
	if got := Host("https://api.anthropic.com/v1/messages"); got != "api.anthropic.com" {
		t.Errorf("Host should be host[:port] only, got %q", got)
	}
}

// An explicit alias names its endpoint: openrouter goes to OpenRouter even when
// the key arrives as the generic PGBOT_AI_API_KEY (the raw-variable check alone
// sent it to api.openai.com), ollama goes to the local default with no key, and
// the generic openai-compatible alias has no endpoint to guess.
func TestResolve_explicitAliasesPickTheirEndpoint(t *testing.T) {
	clearEnv(t)
	t.Setenv("PGBOT_AI_PROVIDER", "openrouter")
	t.Setenv("PGBOT_AI_API_KEY", "sk-or-…")
	m, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if m.Endpoint() != defaultOpenRouterURL {
		t.Errorf("openrouter with PGBOT_AI_API_KEY: endpoint = %q, want %q", m.Endpoint(), defaultOpenRouterURL)
	}

	clearEnv(t)
	t.Setenv("PGBOT_AI_PROVIDER", "ollama")
	m, err = Resolve()
	if err != nil {
		t.Fatalf("ollama with no key and no base URL: %v", err)
	}
	if m.Endpoint() != defaultOllamaURL {
		t.Errorf("ollama: endpoint = %q, want %q", m.Endpoint(), defaultOllamaURL)
	}

	clearEnv(t)
	t.Setenv("PGBOT_AI_PROVIDER", "openai-compatible")
	t.Setenv("PGBOT_AI_API_KEY", "k")
	if _, err := Resolve(); err == nil || !strings.Contains(err.Error(), "PGBOT_AI_BASE_URL") {
		t.Errorf("openai-compatible without a base URL should ask for PGBOT_AI_BASE_URL, got: %v", err)
	}

	clearEnv(t)
	t.Setenv("PGBOT_AI_PROVIDER", "openai")
	t.Setenv("OPENAI_API_KEY", "k")
	m, err = Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if m.Endpoint() != defaultOpenAIURL {
		t.Errorf("explicit openai: endpoint = %q, want %q", m.Endpoint(), defaultOpenAIURL)
	}
}

// requesty is an explicit alias only: it goes to Requesty with REQUESTY_API_KEY
// or PGBOT_AI_API_KEY, never borrows an OpenAI key, and is never auto-detected.
func TestResolve_requesty(t *testing.T) {
	for _, keyVar := range []string{"REQUESTY_API_KEY", "PGBOT_AI_API_KEY"} {
		clearEnv(t)
		t.Setenv("PGBOT_AI_PROVIDER", "requesty")
		t.Setenv(keyVar, "k")
		m, err := Resolve()
		if err != nil {
			t.Fatalf("requesty with %s: %v", keyVar, err)
		}
		if m.Provider() != "openai" {
			t.Errorf("requesty with %s: provider = %q, want openai", keyVar, m.Provider())
		}
		if m.Endpoint() != defaultRequestyURL {
			t.Errorf("requesty with %s: endpoint = %q, want %q", keyVar, m.Endpoint(), defaultRequestyURL)
		}
	}

	clearEnv(t)
	t.Setenv("PGBOT_AI_PROVIDER", "requesty")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	if _, err := Resolve(); err == nil || !strings.Contains(err.Error(), "REQUESTY_API_KEY") {
		t.Errorf("requesty with only OPENAI_API_KEY should ask for REQUESTY_API_KEY, got: %v", err)
	}

	clearEnv(t)
	t.Setenv("REQUESTY_API_KEY", "k")
	if _, err := Resolve(); err == nil {
		t.Error("REQUESTY_API_KEY alone must not be auto-detected")
	}
}

// A custom remote endpoint must come with a named provider. Auto-detecting one
// from an ambient vendor key would send that key — say the ANTHROPIC_API_KEY
// another tool left in the shell — to whatever host PGBOT_AI_BASE_URL names,
// with Anthropic's request format, and --yes would never show it.
func TestResolve_remoteBaseURLNeedsExplicitProvider(t *testing.T) {
	clearEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-…")
	t.Setenv("PGBOT_AI_BASE_URL", "https://gateway.example/v1")
	_, err := Resolve()
	if err == nil {
		t.Fatal("remote PGBOT_AI_BASE_URL with no PGBOT_AI_PROVIDER resolved a model; want an error")
	}
	if !strings.Contains(err.Error(), "PGBOT_AI_PROVIDER") || strings.Contains(err.Error(), "sk-ant") {
		t.Errorf("error should ask for PGBOT_AI_PROVIDER and never echo a key, got: %v", err)
	}

	// Naming it makes the same setup work, and the local case never needed it.
	t.Setenv("PGBOT_AI_PROVIDER", "anthropic")
	if m, err := Resolve(); err != nil || m.Endpoint() != "https://gateway.example/v1" {
		t.Errorf("named provider with a remote base URL: model=%v err=%v", m, err)
	}
	clearEnv(t)
	t.Setenv("PGBOT_AI_BASE_URL", "http://127.0.0.1:8000/v1")
	if _, err := Resolve(); err != nil {
		t.Errorf("local PGBOT_AI_BASE_URL alone should still resolve: %v", err)
	}
}
