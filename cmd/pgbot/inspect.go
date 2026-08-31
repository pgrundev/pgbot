package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/pgrundev/pgbot/internal/collect"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/crdbhttp"
	"github.com/pgrundev/pgbot/internal/diff"
	"github.com/pgrundev/pgbot/internal/events"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/render"
	"github.com/pgrundev/pgbot/internal/store"
	"github.com/spf13/cobra"
)

// Exit-code contract — a public interface people script against (B5). Suppressed
// findings never contribute to it (B2-2). Documented in the README.
const (
	exitClean    = 0  // no findings above info (or all above-info findings suppressed)
	exitWarn     = 1  // at least one warning, no criticals
	exitCritical = 2  // at least one critical finding
	exitFailure  = 3  // connection / execution failure
	exitUsage    = 64 // malformed invocation: bad flags, args, or unknown command (EX_USAGE)
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
	config       string   // explicit .pgbot.toml path ("" = discover)
	ignore       []string // one-off --ignore finding[:object] rules (B2-4)
	failOn       string   // exit non-zero on findings at/above this severity (B5-1)
	format       string   // text|json|sarif|junit (B5-2)
	allDatabases bool     // inspect every database in the cluster (B3)
	parallel     int      // max concurrent database inspections (B3); default 1 = serial
	profile      string   // full (default) | schema: emit only schema-scoped findings (D3-1)
	failOnNew    string   // path to a base report; act only on findings new vs it (D3-2)
	crdbAdminURL string   // CockroachDB DB Console/Admin API origin
	crdbPromURL  string   // CockroachDB Prometheus origin or /_status/load URL
	crdbHTTP     bool     // gather should include CockroachDB Admin/Prometheus collectors
}

// schemaProfile reports whether this run is a schema-only check (--profile=schema).
func (f inspectFlags) schemaProfile() bool { return f.profile == "schema" }

func newInspectCmd() *cobra.Command {
	var f inspectFlags
	cmd := &cobra.Command{
		Use:   "inspect <connection-string>",
		Short: "Collect a full in-database health report",
		Long: "Connect read-only, sample the statistics views, and print a findings-first\n" +
			"report (or --json). Writes a baseline snapshot so later runs show what changed.\n\n" +
			"The connection string may be a URL (postgres://...) or a libpq DSN, or set\n" +
			"$DATABASE_URL and omit the argument. On PostgreSQL use pg_monitor; on CockroachDB\n" +
			"use VIEWACTIVITY. The monitoring role should have no write grants.",
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
	fl.StringVar(&f.failOn, "fail-on", "warn", "exit non-zero on findings at/above this severity: critical|warn|info|none")
	fl.StringVar(&f.format, "format", "text", "output format: text|json|sarif|junit|prometheus")
	fl.StringVar(&f.profile, "profile", "full", "which findings to run: full (a live database) | schema (catalog-only, safe on an empty CI database)")
	fl.StringVar(&f.failOnNew, "fail-on-new", "", "path to a base report (JSON); mark findings already in it preexisting and act only on new ones")
	fl.BoolVar(&f.allDatabases, "all-databases", false, "inspect every database in the cluster (cluster-wide findings reported once)")
	fl.IntVar(&f.parallel, "parallel", 1, "max databases inspected concurrently under --all-databases (default 1 = serial)")
	fl.StringVar(&f.crdbAdminURL, "crdb-admin-url", "", "CockroachDB DB Console/Admin API origin (or PGBOT_CRDB_ADMIN_URL)")
	fl.StringVar(&f.crdbPromURL, "crdb-prometheus-url", "", "CockroachDB Prometheus origin or /_status/load URL (defaults to admin URL)")
	return cmd
}

func runInspect(cmd *cobra.Command, args []string, f inspectFlags) error {
	if !validFailOn(f.failOn) {
		return usageErrf("--fail-on must be critical|warn|info|none, got %q", f.failOn)
	}
	if f.json && f.format == "text" {
		f.format = "json" // --json is a shortcut for --format=json
	}
	if !validFormat(f.format) {
		return usageErrf("--format must be text|json|sarif|junit|prometheus, got %q", f.format)
	}
	if f.profile != "full" && f.profile != "schema" {
		return usageErrf("--profile must be full|schema, got %q", f.profile)
	}
	connString := firstNonEmpty(argAt(args, 0), os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"))
	if connString == "" {
		return fmt.Errorf("no connection string (pass one or set $DATABASE_URL)")
	}
	if f.allDatabases {
		return runInspectAll(cmd.Context(), connString, f)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
	defer cancel()

	target, err := conn.Connect(ctx, connString)
	if err != nil {
		return fmt.Errorf("connect: %s", conn.RedactConnString(err.Error()))
	}
	defer target.Close()
	httpClient, err := newCockroachHTTP(connString, target.Caps, f)
	if err != nil {
		return err
	}
	defer closeCockroachHTTP(httpClient)

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
		SchemaOnly: f.schemaProfile(), CockroachHTTP: httpClient,
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

	switch f.format {
	case "json":
		if err := render.JSON(os.Stdout, c); err != nil {
			return err
		}
	case "sarif":
		if err := render.SARIF(os.Stdout, c); err != nil {
			return err
		}
	case "junit":
		if err := render.JUnit(os.Stdout, c, f.failOn); err != nil {
			return err
		}
	case "prometheus":
		if err := render.Prometheus(os.Stdout, c); err != nil {
			return err
		}
	default:
		host, _ := hostPort(target)
		opts := render.Options{Color: useColor(f.noColor), Trends: trends, BaselinePath: baselinePath, Width: terminalWidth(), Full: f.full, Host: host}
		if err := render.Terminal(os.Stdout, c, opts); err != nil {
			return err
		}
	}

	// os.Exit skips defers, so revoke an API session explicitly on the success
	// path. The defer above still covers every early return.
	closeCockroachHTTP(httpClient)
	os.Exit(exitCode(c.Findings, f.failOn))
	return nil
}

func newCockroachHTTP(connString string, caps conn.Capabilities, f inspectFlags) (*crdbhttp.Client, error) {
	if !caps.IsCockroachDB() {
		return nil, nil
	}
	adminURL := firstNonEmpty(f.crdbAdminURL, os.Getenv("PGBOT_CRDB_ADMIN_URL"))
	promURL := firstNonEmpty(f.crdbPromURL, os.Getenv("PGBOT_CRDB_PROMETHEUS_URL"))
	if adminURL == "" && promURL == "" {
		return nil, nil
	}
	client, err := crdbhttp.NewFromConnectionString(connString, crdbhttp.Config{
		AdminURL: adminURL, PrometheusURL: promURL, APISession: os.Getenv("PGBOT_CRDB_API_SESSION"),
	})
	if err != nil {
		return nil, fmt.Errorf("CockroachDB health endpoints: %s", conn.RedactConnString(err.Error()))
	}
	return client, nil
}

func closeCockroachHTTP(client *crdbhttp.Client) {
	if client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Close(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "pgbot: could not revoke CockroachDB API session: "+conn.RedactConnString(err.Error()))
	}
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

// severityRank orders severities so --fail-on can gate on a threshold.
func severityRank(sev string) int {
	switch sev {
	case model.SeverityCritical:
		return 3
	case model.SeverityWarn:
		return 2
	case model.SeverityInfo:
		return 1
	}
	return 0
}

// exitCode maps findings to the CI contract, gated by --fail-on (B5-1): only
// unsuppressed findings at or above the failOn threshold count. failOn is one of
// critical|warn|info|none; "warn" is the default and reproduces the historical
// behavior (critical→2, warn→1). "none" always returns 0. Suppressed findings
// (B2) never contribute — a muted checksums_disabled must not keep failing CI.
func exitCode(fs []model.Finding, failOn string) int {
	if failOn == "none" {
		return exitClean
	}
	threshold := severityRank(failOn)
	hasCritical, hasAtThreshold := false, false
	for _, f := range fs {
		// Suppressed (B2) and preexisting (D3-2, --fail-on-new) findings never move
		// the exit code — only active, newly-introduced findings do.
		if f.Suppressed || f.Preexisting || severityRank(f.Severity) < threshold {
			continue
		}
		hasAtThreshold = true
		if f.Severity == model.SeverityCritical {
			hasCritical = true
		}
	}
	switch {
	case hasCritical:
		return exitCritical
	case hasAtThreshold:
		return exitWarn
	default:
		return exitClean
	}
}

// validFailOn reports whether s is an accepted --fail-on value.
func validFailOn(s string) bool {
	switch s {
	case "critical", "warn", "info", "none":
		return true
	}
	return false
}

// validFormat reports whether s is an accepted --format value.
func validFormat(s string) bool {
	switch s {
	case "text", "json", "sarif", "junit", "prometheus":
		return true
	}
	return false
}
