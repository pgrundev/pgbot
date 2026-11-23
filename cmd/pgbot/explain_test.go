package main

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
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
