package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pgrundev/pgbot/internal/ai"
	"github.com/pgrundev/pgbot/internal/collect"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/render"
	"github.com/pgrundev/pgbot/internal/store"
	"github.com/spf13/cobra"
)

// newExplainCmd is the OPTIONAL AI layer. It runs the exact same deterministic
// collection as `inspect`, prints that report unchanged (the findings are still
// computed in Go), then asks a model to explain it in plain language. The model
// never generates a finding — it only interprets the ones pgbot computed.
func newExplainCmd() *cobra.Command {
	var f inspectFlags
	var yes bool
	cmd := &cobra.Command{
		Use:   "explain <connection-string>",
		Short: "Inspect, then have an AI explain the findings in plain language",
		Long: "Runs the same read-only inspection as `pgbot inspect`, prints the deterministic\n" +
			"report, then sends the PII-free findings to an AI provider for a plain-language\n" +
			"explanation. The findings are still computed locally in Go — the model only\n" +
			"explains them, never invents them.\n\n" +
			"The key is read from $OPENAI_API_KEY (OpenAI) or $GEMINI_API_KEY (Google Gemini),\n" +
			"never a flag; set PGBOT_AI_PROVIDER to force one. This is the only pgbot command\n" +
			"that sends data off the machine; the payload is the same PII-free Context you can\n" +
			"inspect with `pgbot inspect --json`.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExplain(cmd, args, f, yes)
		},
	}
	fl := cmd.Flags()
	fl.DurationVar(&f.interval, "interval", time.Second, "gap between the two counter samples (min 500ms)")
	fl.BoolVar(&f.noColor, "no-color", false, "disable ANSI color")
	fl.BoolVar(&f.noStore, "no-store", false, "do not read or write the local baseline store")
	fl.StringVar(&f.storePath, "store", "", "baseline DB path (default: XDG state dir)")
	fl.BoolVar(&f.strictPooler, "strict-pooler", false, "refuse (exit 3) if connected through a transaction pooler")
	fl.IntVar(&f.ashHz, "ash-hz", 10, "active-session sampling rate in Hz (0 disables the wait-event profile)")
	fl.DurationVar(&f.window, "window", 5*time.Second, "active-session sampling window")
	fl.BoolVar(&yes, "yes", false, "skip the 'this sends data to the AI provider' confirmation prompt")
	fl.StringVar(&f.config, "config", "", "path to .pgbot.toml (default: discover from cwd upward)")
	fl.StringArrayVar(&f.ignore, "ignore", nil, "suppress a finding for this run: finding[:object] (repeatable)")
	return cmd
}

func runExplain(cmd *cobra.Command, args []string, f inspectFlags, yes bool) error {
	// Build the model client first — fail fast before we connect if the key is
	// missing, so the user isn't surprised after a full inspection.
	client, err := ai.NewFromEnv()
	if err != nil {
		return err
	}

	// This is the one command that sends data off the box. Say so, loudly, and
	// require an explicit go-ahead unless --yes (or non-interactive).
	fmt.Fprintf(os.Stderr, "pgbot explain: this sends the PII-free findings (same as `inspect --json`) to %s (model %s).\n", client.Vendor(), client.ModelName())
	if !yes && isInteractive() {
		fmt.Fprint(os.Stderr, "Continue? [y/N] ")
		var resp string
		fmt.Fscanln(os.Stdin, &resp)
		if r := strings.ToLower(strings.TrimSpace(resp)); r != "y" && r != "yes" {
			return fmt.Errorf("aborted")
		}
	}

	connString := firstNonEmpty(argAt(args, 0), os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"))
	if connString == "" {
		return fmt.Errorf("no connection string (pass one or set $DATABASE_URL)")
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 45*time.Second)
	defer cancel()

	target, err := conn.Connect(ctx, connString)
	if err != nil {
		return fmt.Errorf("connect: %s", conn.RedactConnString(err.Error()))
	}
	defer target.Close()

	if target.Pooler.Detected {
		if f.strictPooler {
			fmt.Fprintln(os.Stderr, target.Pooler.StrictMessage())
			os.Exit(exitFailure)
		}
		fmt.Fprintln(os.Stderr, target.Pooler.Note())
	}

	// Never keep raw query text here — the payload leaves the machine.
	c, err := collect.Run(ctx, target, collect.Options{Interval: f.interval, ASHHz: f.ashHz, ASHWindow: f.window})
	if err != nil {
		return fmt.Errorf("collect: %s", conn.RedactConnString(err.Error()))
	}
	c.Server.ViaPooler = target.Pooler.Detected
	host, port := hostPort(target)
	c.Fingerprint = store.Fingerprint(host, port, c.Server.Database, target.Caps.SystemIdentifier)

	var trends map[string][]float64
	var baselinePath string
	if !f.noStore {
		trends, baselinePath = withStore(f.storePath, c)
	}
	if err := computeFindings(c, f); err != nil {
		return err
	}

	// 1. The deterministic report — the truth, printed exactly as `inspect` would.
	color := useColor(f.noColor)
	opts := render.Options{Color: color, Trends: trends, BaselinePath: baselinePath, Width: terminalWidth(), Host: host}
	if err := render.Terminal(os.Stdout, c, opts); err != nil {
		return err
	}

	// 2. The AI explanation — clearly labeled as model-generated. If it fails, the
	// deterministic report above still stands; we just note the explanation is
	// unavailable and exit on the findings' code.
	explanation, aiErr := ai.Explain(ctx, client, c)
	printAISection(color, client.ModelName(), explanation, aiErr)

	os.Exit(exitCode(c.Findings))
	return nil
}

// printAISection renders the labeled AI block. The banner makes it unmistakable
// that this text is model-generated and must be verified before acting.
func printAISection(color bool, modelName, text string, aiErr error) {
	st := render.NewStyler(color)
	rule := strings.Repeat("─", 64)
	fmt.Println()
	fmt.Println(st.Dim(rule))
	fmt.Println(st.AI(fmt.Sprintf("🤖 AI explanation · generated by %s — verify before acting", modelName)))
	fmt.Println(st.Dim(rule))
	fmt.Println()
	if aiErr != nil {
		fmt.Println(st.Dim("  (explanation unavailable: " + aiErr.Error() + ")"))
		fmt.Println(st.Dim("  The findings above are complete and were computed without the model."))
		return
	}
	fmt.Println(text)
	fmt.Println()
	fmt.Println(st.Dim("The findings above are authoritative and computed deterministically; the text"))
	fmt.Println(st.Dim("in this section is a model's interpretation and may be wrong."))
}
