package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/pgrundev/pgbot/internal/collect"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/diff"
	"github.com/pgrundev/pgbot/internal/events"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/render"
	"github.com/pgrundev/pgbot/internal/store"
	"github.com/spf13/cobra"
)

// Exit-code contract (CI users depend on these):
const (
	exitClean    = 0 // no findings above info
	exitWarn     = 1 // warnings present
	exitCritical = 2 // critical findings present
	exitFailure  = 3 // connection / execution failure
)

type inspectFlags struct {
	json         bool
	interval     time.Duration
	noColor      bool
	noStore      bool
	storePath    string
	rawQueries   bool
	strictPooler bool
	ashHz        int
	window       time.Duration
	full         bool
	timeout      time.Duration
	config       string // explicit .pgbot.toml path ("" = discover)
	ignore       []string // one-off --ignore finding[:object] rules (B2-4)
}

func newInspectCmd() *cobra.Command {
	var f inspectFlags
	cmd := &cobra.Command{
		Use:   "inspect <connection-string>",
		Short: "Collect a full in-database health report",
		Long: "Connect read-only, sample the statistics views, and print a findings-first\n" +
			"report (or --json). Writes a baseline snapshot so later runs show what changed.\n\n" +
			"The connection string may be a URL (postgres://...) or a libpq DSN, or set\n" +
			"$DATABASE_URL and omit the argument. Use a role holding pg_monitor and no write grants.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInspect(cmd, args, f)
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.json, "json", false, "emit the versioned Context as JSON (the agent/script contract)")
	fl.DurationVar(&f.interval, "interval", time.Second, "gap between the two counter samples (min 500ms)")
	fl.BoolVar(&f.noColor, "no-color", false, "disable ANSI color")
	fl.BoolVar(&f.noStore, "no-store", false, "do not read or write the local baseline store")
	fl.StringVar(&f.storePath, "store", "", "baseline DB path (default: XDG state dir)")
	fl.BoolVar(&f.rawQueries, "raw-query-text", false, "keep raw pg_stat_activity query text (never sent anywhere; PII risk)")
	fl.BoolVar(&f.strictPooler, "strict-pooler", false, "refuse (exit 3) if connected through a transaction pooler; default proceeds since rates stay correct")
	fl.IntVar(&f.ashHz, "ash-hz", 10, "active-session sampling rate in Hz (0 disables the wait-event profile)")
	fl.DurationVar(&f.window, "window", 5*time.Second, "active-session sampling window (how long to profile where time goes)")
	fl.BoolVar(&f.full, "full", false, "print the full section tables; default is the sentences-first summary")
	fl.DurationVar(&f.timeout, "timeout", 30*time.Second, "total wall-clock budget for the whole run (raise it for slow or remote databases)")
	fl.StringVar(&f.config, "config", "", "path to .pgbot.toml (default: discover from cwd upward, then $XDG_CONFIG_HOME)")
	fl.StringArrayVar(&f.ignore, "ignore", nil, "suppress a finding for this run: finding[:object] (repeatable)")
	return cmd
}

func runInspect(cmd *cobra.Command, args []string, f inspectFlags) error {
	connString := firstNonEmpty(argAt(args, 0), os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"))
	if connString == "" {
		return fmt.Errorf("no connection string (pass one or set $DATABASE_URL)")
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
	defer cancel()

	target, err := conn.Connect(ctx, connString)
	if err != nil {
		return fmt.Errorf("connect: %s", conn.RedactConnString(err.Error()))
	}
	defer target.Close()

	// Transaction poolers: pgbot's rates stay correct behind one (each counter is
	// read in its own transaction; pg_stat_* are cluster-wide), so we proceed by
	// default and just note it. --strict-pooler refuses for the cautious. We do
	// automatically fall back to the simple wire protocol when the pooler rejects
	// prepared statements (handled in conn.Connect).
	if target.Pooler.Detected {
		if f.strictPooler {
			fmt.Fprintln(os.Stderr, target.Pooler.StrictMessage())
			os.Exit(exitFailure)
		}
		fmt.Fprintln(os.Stderr, target.Pooler.Note())
	}

	// --raw-query-text keeps literal SQL from pg_stat_activity (a PII vector).
	// There is no LLM/remote destination in slice 1, so it only affects local
	// output; warn loudly regardless.
	if f.rawQueries {
		fmt.Fprintln(os.Stderr, "pgbot: --raw-query-text is set — blocking-chain query text is NOT scrubbed and may contain literal values (PII).")
	}
	c, err := collect.Run(ctx, target, collect.Options{
		Interval: f.interval, RawQueryText: f.rawQueries, ASHHz: f.ashHz, ASHWindow: f.window, Deadline: f.timeout,
	})
	if err != nil {
		return fmt.Errorf("collect: %s", conn.RedactConnString(err.Error()))
	}
	c.Server.ViaPooler = target.Pooler.Detected

	// Fingerprint the target so baselines survive host/rename changes.
	host, port := hostPort(target)
	c.Fingerprint = store.Fingerprint(host, port, c.Server.Database, target.Caps.SystemIdentifier)

	// Baseline: diff against history, then persist this run. Store trouble is
	// non-fatal — a broken local DB must never stop a health report.
	var trends map[string][]float64
	var baselinePath string
	if !f.noStore {
		trends, baselinePath = withStore(f.storePath, c)
	}

	// Deterministic findings — computed in Go, never by a model. Under the active
	// .pgbot.toml: threshold overrides feed the compute, severity/ignore rules are
	// applied, suppressed findings are kept (marked) for the renderer.
	if err := computeFindings(c, f); err != nil {
		return err
	}

	if f.json {
		if err := render.JSON(os.Stdout, c); err != nil {
			return err
		}
	} else {
		host, _ := hostPort(target)
		opts := render.Options{Color: useColor(f.noColor), Trends: trends, BaselinePath: baselinePath, Width: terminalWidth(), Full: f.full, Host: host}
		if err := render.Terminal(os.Stdout, c, opts); err != nil {
			return err
		}
	}

	os.Exit(exitCode(c.Findings))
	return nil
}

// withStore loads the baseline (for Deltas + sparkline trends) and persists this
// run. It mutates c.Deltas in place and returns sparkline series + the store
// path for the footer. All store errors are swallowed after a stderr note.
func withStore(path string, c *model.Context) (map[string][]float64, string) {
	st, err := store.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pgbot: baseline store unavailable: "+err.Error())
		return nil, ""
	}
	defer st.Close()

	if notice := st.UpgradeNotice(); notice != "" {
		fmt.Fprintln(os.Stderr, "pgbot: "+notice)
	}

	now := c.CollectedAt
	// The immediately-previous run drives events + reset detection.
	last, _ := st.Previous(c.Fingerprint, now, 0)
	if last != nil {
		// Derive what changed (schema/config/lifecycle) vs the last run.
		prevSchema, _ := st.LoadLatestSchema(c.Fingerprint)
		c.Events = events.Derive(c, prevSchema, settingsOf(last.Context), last.CollectedAt)

		// A stats reset / restart between runs makes every delta fiction — suppress
		// the whole section rather than reporting a wake as a -99.97% change.
		if reason := diff.StatsResetBetween(last.Context, c); reason != "" {
			c.DeltaSuppressedReason = reason
		} else {
			// Prefer a baseline ≥15min old for deltas (avoids same-minute noise);
			// fall back to the last run so two back-to-back inspects still diff.
			baseline := last
			if aged, _ := st.Previous(c.Fingerprint, now, 15*time.Minute); aged != nil {
				baseline = aged
			}
			var yday *diff.Baseline
			if y, err := st.SameHourYesterday(c.Fingerprint, now); err == nil && y != nil {
				yday = &diff.Baseline{CollectedAt: y.CollectedAt, Context: y.Context}
			}
			c.Deltas = diff.Compute(c, &diff.Baseline{CollectedAt: baseline.CollectedAt, Context: baseline.Context}, yday)
		}
	}

	id, err := st.Save(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pgbot: could not write baseline: "+err.Error())
	} else {
		if err := st.SaveSchema(c.Fingerprint, id, c.Schema); err != nil {
			fmt.Fprintln(os.Stderr, "pgbot: could not store schema fingerprint: "+err.Error())
		}
		if err := st.AppendEvents(c.Fingerprint, now, c.Events); err != nil {
			fmt.Fprintln(os.Stderr, "pgbot: could not store events: "+err.Error())
		}
		if err := st.SaveWaitProfile(c.Fingerprint, now, c.WaitProfile); err != nil {
			fmt.Fprintln(os.Stderr, "pgbot: could not store wait profile: "+err.Error())
		}
	}

	trends := map[string][]float64{}
	for _, col := range []string{"tps", "cache_hit", "connections", "db_size_bytes"} {
		if series, err := st.Trend(c.Fingerprint, col, 24); err == nil && len(series) > 1 {
			trends[col] = series
		}
	}
	return trends, st.Path()
}

// settingsOf safely extracts the config-override map from a stored Context.
func settingsOf(c *model.Context) map[string]string {
	if c == nil || c.Settings == nil {
		return nil
	}
	return c.Settings.Overrides
}

// exitCode maps the worst finding severity to the CI contract. Suppressed
// findings (B2) never contribute — that is the entire point of an exit-code
// suppression: a muted checksums_disabled must not keep failing CI.
func exitCode(fs []model.Finding) int {
	worst := exitClean
	for _, f := range fs {
		if f.Suppressed {
			continue
		}
		switch f.Severity {
		case model.SeverityCritical:
			return exitCritical
		case model.SeverityWarn:
			worst = exitWarn
		}
	}
	return worst
}
