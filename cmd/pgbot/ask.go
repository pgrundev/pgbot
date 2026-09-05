package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pgrundev/pgbot/internal/ai"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/render"
	"github.com/spf13/cobra"
)

// newAskCmd — `pgbot ask "why is it slow?"`. Collects the same read-only report,
// then answers the question grounded ONLY on those deterministic findings. Like
// `explain` but question-driven and AI-first (no report printed above).
func newAskCmd() *cobra.Command {
	var f inspectFlags
	var yes bool
	var url string
	cmd := &cobra.Command{
		Use:   `ask "<question>"`,
		Short: "Ask an AI about your database, grounded on pgbot's findings",
		Long: "Runs the same read-only inspection, then answers your question using ONLY the\n" +
			"deterministic findings (the model can't reach into the database). Connection\n" +
			"comes from --url or $DATABASE_URL. Sends the PII-free findings to the model you\n" +
			"configured — Gemini, Anthropic, OpenAI, xAI, or an OpenAI-compatible endpoint.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAsk(cmd, strings.Join(args, " "), url, f, yes)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&url, "url", "", "connection string (else $DATABASE_URL)")
	fl.DurationVar(&f.window, "window", 5*time.Second, "active-session sampling window")
	fl.IntVar(&f.ashHz, "ash-hz", 10, "active-session sampling rate in Hz (0 disables)")
	fl.BoolVar(&f.noStore, "no-store", false, "do not read or write the local baseline store")
	fl.BoolVar(&f.strictPooler, "strict-pooler", false, "refuse (exit 3) behind a transaction pooler")
	fl.BoolVar(&yes, "yes", false, "skip the data-disclosure confirmation prompt")
	return cmd
}

func runAsk(cmd *cobra.Command, question, url string, f inspectFlags, yes bool) error {
	llm, err := ai.Resolve()
	if err != nil {
		return err
	}
	if !confirmDisclosure("pgbot ask", llm, yes) {
		return fmt.Errorf("aborted")
	}

	connString := firstNonEmpty(url, os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"))
	if connString == "" {
		return fmt.Errorf("no connection string (pass --url or set $DATABASE_URL)")
	}
	f.interval = time.Second

	ctx, cancel := context.WithTimeout(cmd.Context(), 45*time.Second)
	defer cancel()

	c, _, err := gather(ctx, connString, f)
	if err != nil {
		return err
	}

	// Give the model its own deadline instead of the remainder of collection's
	// budget. Local models may need substantially longer than hosted providers.
	aiCtx, aiCancel := context.WithTimeout(cmd.Context(), 3*time.Minute)
	defer aiCancel()
	answer, aiErr := ai.Ask(aiCtx, llm, c, question)
	printAnswer(useColor(false), llm.Model(), answer, aiErr)
	if aiErr == nil {
		// `ask` prints only the model's prose, so a destructive-action guard that
		// the model may have reworded away must be reasserted here, verbatim from
		// the deterministic findings — never at the model's discretion.
		printSafetyFooter(useColor(false), c)
	}
	return nil
}

// printSafetyFooter reasserts, from code, every destructive-action guard attached
// to a non-suppressed finding — the warnings a summarizing model must not lose. It
// is rendered outside the model's output so the model has no way to omit or reword
// it. Deduped by guard ID.
func printSafetyFooter(color bool, c *model.Context) {
	var lines []string
	seen := map[string]bool{}
	for _, f := range c.Findings {
		if f.Suppressed || f.Safety == nil {
			continue
		}
		for _, g := range f.Safety.BlockingCaveats {
			if seen[g.ID] {
				continue
			}
			seen[g.ID] = true
			line := "[" + g.Action + "] " + g.Text
			if g.Verify != nil {
				line += " Only after: " + *g.Verify
			}
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return
	}
	st := render.NewStyler(color)
	fmt.Println()
	fmt.Println(st.Warn("⚠ Safety (from pgbot, not the model) — a destructive action is in scope for this database:"))
	for _, l := range lines {
		fmt.Println(st.Dim("  • " + l))
	}
}

// printAnswer renders `ask` cleanly: the answer itself, then one dim line making
// clear it's a model's reading of pgbot's deterministic findings. No heavy banner
// — for `ask` the whole command IS the AI, so a full separator would be noise.
func printAnswer(color bool, modelName, text string, aiErr error) {
	st := render.NewStyler(color)
	fmt.Println()
	if aiErr != nil {
		fmt.Println(st.Dim("(no AI answer: " + aiErr.Error() + ")"))
		fmt.Println(st.Dim("Run `pgbot inspect` for the deterministic findings — they need no model."))
		return
	}
	fmt.Println(text)
	fmt.Println()
	fmt.Println(st.Dim("— " + modelName + " · a reading of pgbot's findings; verify before acting"))
}
