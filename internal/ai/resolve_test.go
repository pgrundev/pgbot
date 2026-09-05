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
		"OPENAI_API_KEY", "OPENROUTER_API_KEY", "XAI_API_KEY", "GROK_API_KEY",
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
	t.Setenv("PGBOT_AI_PROVIDER", "bedrock")
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
