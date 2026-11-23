package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/ai"
)

// captureStdout returns what fn printed to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	return <-done
}

// stubModel is an ai.LanguageModel that only answers the consent questions.
type stubModel struct{ endpoint string }

func (stubModel) Generate(context.Context, ai.Call) (*ai.Response, error) {
	return nil, errors.New("stubModel.Generate must not be called")
}
func (stubModel) Provider() string   { return "openai" }
func (stubModel) Model() string      { return "stub" }
func (m stubModel) Endpoint() string { return m.endpoint }

// With no terminal to answer the prompt, consent to send data to a remote
// provider must default to NO unless --yes was given (#143). Local endpoints
// keep auto-consent: nothing leaves the machine.
func TestConfirmDisclosureNonInteractive(t *testing.T) {
	orig := isInteractive
	isInteractive = func() bool { return false }
	defer func() { isInteractive = orig }()

	remote := stubModel{endpoint: "https://api.openai.com/v1"}
	if confirmDisclosureOf("pgbot sql explain", "your SQL text and its query plan", remote, false) {
		t.Error("non-interactive without --yes must not consent to a remote send")
	}
	if confirmDisclosure("pgbot explain", remote, false) {
		t.Error("explain: non-interactive without --yes must not consent to a remote send")
	}
	if !confirmDisclosureOf("pgbot sql explain", "your SQL text and its query plan", remote, true) {
		t.Error("--yes must consent")
	}
	if !confirmDisclosureOf("pgbot sql explain", "your SQL text and its query plan", stubModel{endpoint: "http://127.0.0.1:11434/v1"}, false) {
		t.Error("a local endpoint needs no consent")
	}
}

// With no model configured, `pgbot explain` must still run the inspection and
// print the deterministic report: the missing key becomes the AI section's
// "unavailable" reason, not a command error that skips the report (#156).
func TestResolveExplainModelWithoutKey(t *testing.T) {
	for _, k := range []string{"PGBOT_AI_PROVIDER", "PGBOT_AI_API_KEY", "PGBOT_AI_BASE_URL", "PGBOT_OPENAI_URL", "PGBOT_GEMINI_URL",
		"GEMINI_API_KEY", "GOOGLE_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY",
		"XAI_API_KEY", "GROK_API_KEY", "AWS_BEARER_TOKEN_BEDROCK"} {
		t.Setenv(k, "")
	}
	llm, unavailable, err := resolveExplainModel(false)
	if err != nil {
		t.Fatalf("a missing key must not fail the command (that skips the report): %v", err)
	}
	if llm != nil || unavailable == nil {
		t.Fatalf("want no model and an unavailable reason, got llm=%v reason=%v", llm, unavailable)
	}
	out := captureStdout(t, func() { printAISection(false, "", "", unavailable, aiOnFindings) })
	if !strings.Contains(out, "explanation unavailable") || !strings.Contains(out, "The findings above are complete") {
		t.Errorf("AI section should degrade, got:\n%s", out)
	}
}

// `sql explain --ai` renders a query plan, not findings, so its AI section must
// not tell the reader that "the findings above" are complete or authoritative
// (#140). The findings commands keep their wording.
func TestPrintAISectionWording(t *testing.T) {
	for _, aiErr := range []error{nil, errors.New("boom")} {
		plan := captureStdout(t, func() { printAISection(false, "m", "prose", aiErr, aiOnPlan) })
		if strings.Contains(plan, "findings") {
			t.Errorf("plan AI section (err=%v) mentions findings:\n%s", aiErr, plan)
		}
		if !strings.Contains(plan, "query plan above") {
			t.Errorf("plan AI section (err=%v) should refer to the query plan:\n%s", aiErr, plan)
		}
		findings := captureStdout(t, func() { printAISection(false, "m", "prose", aiErr, aiOnFindings) })
		if !strings.Contains(findings, "The findings above are") {
			t.Errorf("findings AI section (err=%v) lost its wording:\n%s", aiErr, findings)
		}
	}
}
