package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pgrundev/pgbot/internal/config"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
	"github.com/pgrundev/pgbot/internal/monitor"
	"github.com/pgrundev/pgbot/internal/render"
	"github.com/pgrundev/pgbot/internal/store"
	"github.com/pgrundev/pggo"
	"github.com/spf13/cobra"
)

// PgBot Monitor: `pgbot monitor` sets up scheduled, read-only health checks on
// this Mac (launchd LaunchAgent → `pgbot monitor _run` → the inspect pipeline
// → Notification Center). See internal/monitor for the moving parts.

// monitorEnv is everything a monitor command needs; tests swap the platform.
type monitorEnv struct {
	paths monitor.Paths
	plat  monitor.Platform
	ui    *ui
}

func newMonitorEnv() (*monitorEnv, error) {
	if !monitor.Supported() {
		return nil, monitor.ErrUnsupported
	}
	p, err := monitor.DefaultPaths()
	if err != nil {
		return nil, err
	}
	return &monitorEnv{paths: p, plat: monitor.NativePlatform(p), ui: newUI()}, nil
}

// monitorRun wraps a handler with the macOS check and env construction.
func monitorRun(fn func(cmd *cobra.Command, e *monitorEnv) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		e, err := newMonitorEnv()
		if err != nil {
			return err
		}
		return fn(cmd, e)
	}
}

func newMonitorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "monitor",
		Short: "Scheduled health checks with native macOS notifications (macOS only)",
		Long: "PgBot Monitor checks your database on a schedule and notifies you in\n" +
			"Notification Center when something needs attention. It runs from a launchd\n" +
			"LaunchAgent on this Mac — no daemon, no cloud, no account. Checks run only\n" +
			"while the Mac is awake and you are logged in; it is not 24/7 server monitoring.\n\n" +
			"Run `pgbot monitor` in a terminal to set it up (or to manage an existing setup).",
		Args: cobra.NoArgs,
		RunE: monitorRun(runMonitorHome),
	}
	var purge bool
	stop := &cobra.Command{
		Use:   "stop",
		Short: "Unregister scheduled monitoring (asks whether to delete saved settings)",
		Args:  cobra.NoArgs,
		RunE: monitorRun(func(cmd *cobra.Command, e *monitorEnv) error {
			return e.stop(purge, cmd.Flags().Changed("purge"))
		}),
	}
	stop.Flags().BoolVar(&purge, "purge", false, "also delete monitor settings, state and the Keychain item (never the baseline history)")

	cmd.AddCommand(
		&cobra.Command{Use: "status", Short: "Show monitor status, last result and next check", Args: cobra.NoArgs,
			RunE: monitorRun(func(_ *cobra.Command, e *monitorEnv) error { return e.status() })},
		&cobra.Command{Use: "now", Short: "Run a health check immediately", Args: cobra.NoArgs,
			RunE: monitorRun(func(cmd *cobra.Command, e *monitorEnv) error { return e.now(cmd.Context()) })},
		&cobra.Command{Use: "test", Short: "Send a test notification", Args: cobra.NoArgs,
			RunE: monitorRun(func(_ *cobra.Command, e *monitorEnv) error { return e.test() })},
		&cobra.Command{Use: "edit", Short: "Change the database, schedule or notifications", Args: cobra.NoArgs,
			RunE: monitorRun(func(cmd *cobra.Command, e *monitorEnv) error {
				cfg, err := monitor.LoadConfig(e.paths.Config)
				if err != nil {
					return err
				}
				return e.wizard(cmd.Context(), cfg)
			})},
		&cobra.Command{Use: "pause", Short: "Pause scheduled checks (settings are kept)", Args: cobra.NoArgs,
			RunE: monitorRun(func(_ *cobra.Command, e *monitorEnv) error { return e.pause() })},
		&cobra.Command{Use: "resume", Short: "Resume scheduled checks (also repairs a moved pgbot binary)", Args: cobra.NoArgs,
			RunE: monitorRun(func(_ *cobra.Command, e *monitorEnv) error { return e.resume() })},
		stop,
		&cobra.Command{Use: "_run", Hidden: true, Short: "Run one scheduled check (invoked by launchd)", Args: cobra.NoArgs,
			RunE: monitorRun(func(cmd *cobra.Command, e *monitorEnv) error { return e.scheduledRun(cmd.Context()) })},
	)
	return cmd
}

// runMonitorHome: the wizard on first use, otherwise status + a small menu.
func runMonitorHome(cmd *cobra.Command, e *monitorEnv) error {
	cfg, err := monitor.LoadConfig(e.paths.Config)
	if errors.Is(err, monitor.ErrNotConfigured) {
		if !e.ui.tty {
			return errors.New("`pgbot monitor` setup is interactive — run it in a terminal")
		}
		return e.wizard(cmd.Context(), nil)
	}
	if err != nil {
		return err
	}
	if err := e.status(); err != nil {
		return err
	}
	if !e.ui.tty {
		return nil
	}
	pauseLabel := "Pause monitoring"
	if cfg.Paused {
		pauseLabel = "Resume monitoring"
	}
	i, err := e.ui.choose("What would you like to do?", []string{
		"Run a check now", "Edit settings", "Send a test notification", pauseLabel, "Done",
	}, 4)
	if errors.Is(err, errAborted) {
		return nil
	}
	if err != nil {
		return err
	}
	switch i {
	case 0:
		return e.now(cmd.Context())
	case 1:
		return e.wizard(cmd.Context(), cfg)
	case 2:
		return e.test()
	case 3:
		if cfg.Paused {
			return e.resume()
		}
		return e.pause()
	}
	return nil
}

// ---- setup wizard -------------------------------------------------------------

// wizard runs first-time setup (existing == nil) or `edit` with the current
// values preselected. Nothing is written until the final confirmation.
func (e *monitorEnv) wizard(ctx context.Context, existing *monitor.Config) error {
	u := e.ui
	if !u.tty {
		return errors.New("the monitor setup is interactive — run it in a terminal")
	}
	program, err := durableExecutable()
	if err != nil {
		return err
	}

	if existing == nil {
		u.header("PgBot Monitor")
		u.println("Your PostgreSQL health assistant.")
		u.println()
		u.println("I'll check your database automatically")
		u.println("and notify you when something needs attention.")
		u.println()
		u.dim("No cloud. No agents. Just Postgres.")
		u.println()
		u.println("Let's get started!")
	} else {
		u.header("Edit PgBot Monitor")
	}

	// Step 1 — database.
	u.println()
	u.println(u.st.Head("Step 1 of 3 · Database"))
	u.println()
	connString, probe, err := e.chooseConnection(ctx, existing)
	if err != nil {
		return err
	}

	// Step 2 — schedule.
	def := monitor.DefaultSchedule()
	if existing != nil {
		def = existing.Schedule
	}
	u.println(u.st.Head("Step 2 of 3 · Schedule"))
	u.println()
	sched, err := e.chooseSchedule(def)
	if err != nil {
		return err
	}

	// Step 3 — notifications.
	u.println(u.st.Head("Step 3 of 3 · Notifications"))
	u.println()
	policies := []monitor.Policy{monitor.PolicyImportant, monitor.PolicyEvery, monitor.PolicyCritical}
	pdef := 0
	if existing != nil {
		for i, p := range policies {
			if p == existing.Policy {
				pdef = i
			}
		}
	}
	pi, err := u.choose("When should I notify you?", []string{
		"Important changes only (Recommended)", "Every health check", "Critical issues only",
	}, pdef)
	if err != nil {
		return err
	}
	policy := policies[pi]

	// Credential consent — explicit, every time a connection is (re)saved.
	if connString != "" {
		u.println(u.st.Head("Saving your connection"))
		u.println()
		u.println("To check while you're away, PgBot keeps this connection in your")
		u.println("macOS login Keychain. It is never written to a file, a log, or the")
		u.println("schedule, and you can remove it any time with `pgbot monitor stop`.")
		u.println()
		ok, err := u.confirm("Save the connection in your Keychain?", true)
		if err != nil {
			return err
		}
		if !ok {
			u.println()
			u.dim("Unattended checks need the saved connection, so setup stops here. Nothing was changed.")
			return nil
		}
		u.println()
	}

	// Confirm.
	cfg := &monitor.Config{}
	if existing != nil {
		*cfg = *existing
	} else {
		cfg.CreatedAt = time.Now()
	}
	cfg.Paused = false
	cfg.Schedule = sched
	cfg.Policy = policy
	cfg.Program = program
	cfg.Env = monitor.CaptureEnv()
	if existing != nil && len(existing.Env) > 0 && len(cfg.Env) == 0 {
		cfg.Env = existing.Env // `edit` from a bare shell keeps what setup captured
	}
	if probe != nil {
		cfg.Database = *probe
	}
	if c, err := config.Load(""); err == nil && c.Source != "" {
		if abs, err := filepath.Abs(c.Source); err == nil {
			cfg.PgbotConfig = abs
		}
	}
	cfg.UpdatedAt = time.Now()

	u.header("Ready to monitor!")
	u.kv("Database", cfg.Database.Name)
	u.kv("Frequency", sched.FrequencyLabel())
	if t := sched.TimeLabel(); t != "" {
		u.kv("Time", t)
	}
	u.kv("Notifications", policy.Describe())
	u.println()
	ok, err := u.confirm("Start monitoring?", true)
	if err != nil {
		return err
	}
	if !ok {
		u.dim("Nothing was changed.")
		return nil
	}
	u.println()
	return e.apply(ctx, cfg, connString, existing == nil)
}

// chooseConnection returns the connection string to save ("" = keep the
// Keychain's current one) and the probed, non-secret database identity.
func (e *monitorEnv) chooseConnection(ctx context.Context, existing *monitor.Config) (string, *monitor.Database, error) {
	u := e.ui
	if existing != nil {
		u.println(u.st.Good("✓") + " Currently monitoring")
		u.println()
		u.kv("Database", existing.Database.Name)
		if existing.Database.Host != "" {
			u.kv("Host", existing.Database.Host)
		}
		u.println()
		keep, err := u.confirm("Keep this database?", true)
		if err != nil {
			return "", nil, err
		}
		u.println()
		if keep {
			return "", nil, nil
		}
	}

	candidate := firstNonEmpty(os.Getenv("DATABASE_URL"), os.Getenv("PGBOT_DATABASE_URL"), pgServiceFallback())
	if candidate != "" {
		if d, err := describeConn(candidate); err == nil {
			u.ok("Database connection found")
			u.println()
			if d.Host != "" {
				u.kv("Host", d.Host)
			}
			u.kv("Database", firstNonEmpty(d.Name, "(server default)"))
			u.println()
			use, err := u.confirm("Use this database?", true)
			if err != nil {
				return "", nil, err
			}
			u.println()
			if !use {
				candidate = ""
			}
		} else {
			candidate = ""
		}
	}

	for {
		if candidate == "" {
			u.println(u.st.Head("PostgreSQL connection"))
			u.println()
			s, err := u.secret("Paste your PostgreSQL connection URL:")
			if err != nil {
				return "", nil, err
			}
			if s == "" {
				u.dim("  A URL looks like postgres://pgbot_ro@db.example.com:5432/production")
				u.println()
				continue
			}
			candidate = s
		}
		var d *monitor.Database
		var caps conn.Capabilities
		err := u.spin("Connecting…", func() error {
			var err error
			d, caps, err = probeConn(ctx, candidate)
			return err
		})
		if err == nil {
			u.ok("Connected successfully!")
			u.println()
			u.kv("PostgreSQL", firstNonEmpty(d.Version, caps.VersionText))
			u.kv("Database", d.Name)
			access := "Read-only sessions · pg_monitor " + u.st.Good("✓")
			if !caps.HasPgMonitor {
				access = "Read-only sessions · " + u.st.Warn("pg_monitor missing")
			}
			u.kv("Access", access)
			u.println()
			if !caps.HasPgMonitor {
				lines, _ := initVerifyReport(caps)
				for _, l := range lines {
					u.dim(l)
				}
				u.println()
				u.dim("Without pg_monitor, checks see only part of the statistics and PgBot")
				u.dim("will never report the database as fully healthy. `pgbot init` prints")
				u.dim("the read-only role setup SQL for an admin to review and run.")
				u.println()
				cont, err := u.confirm("Continue with this role anyway?", false)
				if err != nil {
					return "", nil, err
				}
				u.println()
				if !cont {
					candidate = ""
					continue
				}
			}
			return candidate, d, nil
		}
		u.bad("Could not connect")
		u.dim("  " + monitor.Redact(err.Error()))
		u.dim("  " + connectHint(err))
		u.println()
		again, cerr := u.confirm("Try a different connection?", true)
		if cerr != nil || !again {
			return "", nil, errAborted
		}
		u.println()
		candidate = ""
	}
}

func connectHint(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "password") || strings.Contains(msg, "authentication"):
		return "Check the user and password (a ~/.pgpass entry also works)."
	case strings.Contains(msg, "no such host") || strings.Contains(msg, "lookup"):
		return "The host name didn't resolve — check for typos or VPN."
	case strings.Contains(msg, "refused") || strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline"):
		return "The server didn't answer — is it running and reachable from this Mac?"
	case strings.Contains(msg, "ssl") || strings.Contains(msg, "tls") || strings.Contains(msg, "certificate"):
		return "TLS failed — try adding ?sslmode=require (or verify-full with the CA)."
	case strings.Contains(msg, "parse"):
		return "That doesn't look like a connection URL: postgres://user@host:5432/db"
	}
	return "`pgbot init --verify <url>` checks a monitoring role's prerequisites."
}

// describeConn parses (without connecting) for the "connection found" screen.
func describeConn(cs string) (*monitor.Database, error) {
	cfg, err := pggo.ParseConfig(cs)
	if err != nil {
		return nil, err
	}
	return &monitor.Database{Name: cfg.Database, Host: cfg.Host, Port: cfg.Port, User: cfg.User}, nil
}

// probeConn connects read-only exactly like inspect does and returns the
// database identity. It runs no diagnostics.
func probeConn(ctx context.Context, cs string) (*monitor.Database, conn.Capabilities, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	t, err := conn.Connect(ctx, cs)
	if err != nil {
		return nil, conn.Capabilities{}, errors.New(conn.RedactConnString(err.Error()))
	}
	defer t.Close()
	d, _ := describeConn(cs)
	if d == nil {
		d = &monitor.Database{}
	}
	d.Name = t.Caps.Database
	d.Version = monitor.PGVersion(t.Caps.VersionNum)
	host, port := hostPort(t)
	d.Fingerprint = store.Fingerprint(host, port, t.Caps.Database, t.Caps.SystemIdentifier)
	return d, t.Caps, nil
}

func (e *monitorEnv) chooseSchedule(def monitor.Schedule) (monitor.Schedule, error) {
	u := e.ui
	kinds := []monitor.ScheduleKind{monitor.Hourly, monitor.Every6h, monitor.Daily, monitor.Weekly}
	di := 2
	for i, k := range kinds {
		if k == def.Kind {
			di = i
		}
	}
	i, err := u.choose("How often should I check your database?", []string{
		"Every hour", "Every 6 hours", "Every day (Recommended)", "Every week",
	}, di)
	if err != nil {
		return monitor.Schedule{}, err
	}
	s := monitor.Schedule{Kind: kinds[i]}
	if !s.Calendar() {
		u.ok("Schedule: " + s.Describe())
		u.println()
		return s, nil
	}
	if s.Kind == monitor.Weekly {
		days := []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday}
		names := make([]string, len(days))
		dd := 0
		for j, d := range days {
			names[j] = d.String()
			if def.Kind == monitor.Weekly && d == def.Weekday {
				dd = j
			}
		}
		j, err := u.choose("Which day?", names, dd)
		if err != nil {
			return monitor.Schedule{}, err
		}
		s.Weekday = days[j]
	}
	defTime := monitor.DefaultSchedule().TimeLabel()
	if def.Calendar() {
		defTime = def.TimeLabel()
	}
	q := "What time should I check?"
	if s.Kind == monitor.Weekly {
		q = "What time?"
	}
	u.println(u.st.Head(q))
	u.println()
	for {
		in, err := u.line("Time", defTime)
		if err != nil {
			return monitor.Schedule{}, err
		}
		hh, mm, perr := monitor.ParseClock(in)
		if perr != nil {
			u.dim("  " + perr.Error())
			continue
		}
		s.Hour, s.Minute = hh, mm
		break
	}
	zone, _ := time.Now().Zone()
	u.println()
	u.ok("Schedule: " + s.Describe() + u.st.Dim(" ("+zone+", this Mac's time zone)"))
	u.println()
	return s, nil
}

// apply persists everything and starts monitoring: Keychain, config, launchd,
// first check, notification test, summary.
func (e *monitorEnv) apply(ctx context.Context, cfg *monitor.Config, connString string, firstSetup bool) error {
	u := e.ui
	if connString != "" {
		if err := e.plat.Secrets.Set(e.paths.KeychainSvc, monitor.KeychainAccount, connString); err != nil {
			u.bad("Could not save the connection")
			return err
		}
		u.ok("Connection saved in your Keychain")
	} else if _, err := e.plat.Secrets.Get(e.paths.KeychainSvc, monitor.KeychainAccount); err != nil {
		return fmt.Errorf("the saved connection can't be read (%v) — choose the database again with `pgbot monitor edit`", err)
	}
	if connString != "" && e.fingerprintChanged(cfg) {
		// A different database: its history isn't comparable with the old one.
		_ = os.Remove(e.paths.State)
	}
	if err := monitor.SaveConfig(e.paths.Config, cfg); err != nil {
		return err
	}
	u.ok("Settings saved")

	if err := e.install(cfg); err != nil {
		cfg.Paused = true
		_ = monitor.SaveConfig(e.paths.Config, cfg)
		u.bad("Could not install the schedule")
		return err
	}
	u.ok("Monitoring schedule installed " + u.st.Dim("("+cfg.Schedule.Describe()+")"))

	var rep *monitor.Report
	err := u.spin("Running the first health check (about 10 seconds)…", func() error {
		var err error
		rep, err = e.runner(cfg).Check(ctx, true)
		return err
	})
	switch {
	case err != nil:
		u.bad("First health check could not run: " + monitor.Redact(err.Error()))
	case rep.Err != nil:
		u.bad("First health check failed: " + rep.Err.Error())
	default:
		u.ok("First health check completed")
	}

	ready := monitor.Notification{
		Title: "🐘 PgBot Monitor is ready",
		Body:  "Your database will be checked " + lowerFirst(cfg.Schedule.Describe()) + ".",
	}
	u.dim("macOS may ask whether “PgBot” can send notifications — click Allow.")
	if nerr := e.plat.Notifier.Notify(ready, cfg.Sound); nerr != nil {
		u.warn("Notification could not be requested: " + nerr.Error())
	} else {
		u.ok("Notification test requested")
	}

	if firstSetup || !cfg.NotificationsConfirmed {
		u.println()
		saw, cerr := u.confirm("Did a PgBot notification appear in the corner of your screen?", true)
		if cerr == nil {
			cfg.NotificationsConfirmed = saw
			_ = monitor.SaveConfig(e.paths.Config, cfg)
			if !saw {
				u.println()
				e.notificationHelp()
			}
		}
	}

	u.println()
	u.rule()
	u.println()
	u.println(u.st.Head("🐘 PgBot is watching your Postgres!"))
	u.println()
	if rep != nil && rep.Err == nil && rep.Decision.State.LastSuccess != nil {
		e.printHealthLines(rep.Decision.State.LastSuccess)
	}
	st, _ := monitor.LoadState(e.paths.State)
	u.println("Next check: " + e.nextCheck(cfg, st))
	u.println()
	u.println("You can safely close this terminal.")
	u.dim("Checks run while this Mac is awake and you're logged in.")
	u.dim("Status: pgbot monitor status · Stop: pgbot monitor stop")
	u.println()
	return nil
}

func (e *monitorEnv) fingerprintChanged(cfg *monitor.Config) bool {
	st, err := monitor.LoadState(e.paths.State)
	if err != nil || st.LastSuccess == nil {
		return false
	}
	return cfg.Database.Fingerprint != "" && st.LastSuccess.Fingerprint != cfg.Database.Fingerprint
}

func (e *monitorEnv) notificationHelp() {
	u := e.ui
	u.println(u.st.Head("Turning on notifications"))
	u.println("  1. Open System Settings → Notifications.")
	u.println("  2. Find “PgBot” (the elephant icon) in the app list.")
	u.println("  3. Turn on Allow Notifications and pick the Banners or Alerts style.")
	u.println("  Also check that a Focus mode isn't silencing notifications.")
	u.dim("  (If PgBot isn't listed, look under “Script Editor” — pgbot falls back to it.)")
	u.println("  Then run: pgbot monitor test")
	u.println()
}

// install writes and loads the LaunchAgent for cfg.
func (e *monitorEnv) install(cfg *monitor.Config) error {
	if err := os.MkdirAll(e.paths.LogDir, 0o700); err != nil {
		return err
	}
	plist, err := monitor.Plist(monitor.AgentSpec{
		Label: e.paths.Label, Program: cfg.Program, Schedule: cfg.Schedule, Log: e.paths.Log, Env: cfg.Env,
	})
	if err != nil {
		return err
	}
	return e.plat.Agents.Install(e.paths.Label, e.paths.Plist, plist)
}

func (e *monitorEnv) runner(cfg *monitor.Config) monitor.Runner {
	return monitor.Runner{Paths: e.paths, Platform: e.plat, Inspect: monitorInspector(cfg.PgbotConfig)}
}

// monitorInspector runs the SAME pipeline as `pgbot inspect` (connect, collect,
// baseline store, findings under .pgbot.toml) with inspect's default tunings,
// and hands back the typed Context — no output is rendered or parsed.
func monitorInspector(pgbotConfig string) monitor.Inspector {
	return func(ctx context.Context, cs string) (*monitor.InspectResult, error) {
		f := inspectFlags{
			interval: time.Second, ashHz: 10, window: 5 * time.Second, timeout: 90 * time.Second,
			profile: "full", failOn: "warn", format: "json", config: pgbotConfig,
		}
		if f.config != "" {
			if _, err := os.Stat(f.config); err != nil {
				f.config = "" // the .pgbot.toml moved; fall back to discovery rather than failing every run
			}
		}
		target, err := conn.Connect(ctx, cs)
		if err != nil {
			return nil, fmt.Errorf("connect: %s", conn.RedactConnString(err.Error()))
		}
		defer target.Close()
		c, _, _, err := collectAndAnalyze(ctx, target, f)
		if err != nil {
			return nil, err
		}
		return &monitor.InspectResult{Context: c, Score: render.HealthScore(c), HasPgMonitor: target.Caps.HasPgMonitor}, nil
	}
}

// ---- management commands -------------------------------------------------------

func (e *monitorEnv) status() error {
	u := e.ui
	cfg, err := monitor.LoadConfig(e.paths.Config)
	if err != nil {
		return err
	}
	st, serr := monitor.LoadState(e.paths.State)
	if st == nil {
		return serr
	}
	loaded := e.plat.Agents.Loaded(e.paths.Label)

	u.header("PgBot Monitor")
	switch {
	case cfg.Paused:
		u.kv("Status", u.st.Warn("●")+" Paused")
	case loaded:
		u.kv("Status", u.st.Good("●")+" Active")
	default:
		u.kv("Status", u.st.Crit("●")+" Not scheduled "+u.st.Dim("(run: pgbot monitor resume)"))
	}
	u.kv("Database", cfg.Database.Name)
	if cfg.Database.Version != "" {
		u.kv("PostgreSQL", cfg.Database.Version)
	}
	u.kv("Schedule", cfg.Schedule.Describe())
	notif := cfg.Policy.Describe()
	if !cfg.NotificationsConfirmed {
		notif += u.st.Dim(" (delivery not yet confirmed — pgbot monitor test)")
	}
	u.kv("Notifications", notif)
	u.println()

	if st.LastAttempt.IsZero() {
		u.kv("Last check", "Not yet checked")
	} else {
		u.kv("Last check", monitor.HumanWhen(st.LastAttempt, time.Now()))
	}
	switch {
	case st.Failure != nil:
		u.kv("Result", u.st.Crit("Check failed")+u.st.Dim(" · "+st.Failure.LastErr))
		if st.LastSuccess != nil {
			u.kv("Last good", fmt.Sprintf("%s · %s", monitor.HumanWhen(st.LastSuccess.At, time.Now()), healthWord(u, st.LastSuccess)))
		}
	case st.LastSuccess != nil:
		u.kv("Health", healthWord(u, st.LastSuccess))
	default:
		u.kv("Health", u.st.Dim("Not yet checked"))
	}
	if !cfg.Paused && loaded {
		u.kv("Next check", e.nextCheck(cfg, st))
	}
	u.println()

	// The schedule points at a binary path; surface it if that path is gone.
	if data, err := os.ReadFile(e.paths.Plist); err == nil && !cfg.Paused {
		if prog := monitor.ProgramFromPlist(data); prog != "" {
			if _, err := os.Stat(prog); err != nil {
				u.warn("The scheduled pgbot binary is missing (" + prog + ").")
				u.dim("  Moved or uninstalled? Run `pgbot monitor resume` to point the schedule at this pgbot.")
				u.println()
			}
		}
	}
	if serr != nil {
		u.warn(serr.Error())
		u.println()
	}

	if len(st.Activity) > 0 {
		u.println(u.st.Head("Recent activity"))
		for i := len(st.Activity) - 1; i >= 0 && i >= len(st.Activity)-5; i-- {
			a := st.Activity[i]
			u.printf("%s %s %s\n", activityIcon(u, a.Kind), a.Text, u.st.Dim("· "+monitor.HumanWhen(a.At, time.Now())))
		}
		u.println()
	}
	if st.LastSuccess != nil && len(st.LastSuccess.Findings) > 0 {
		u.println(u.st.Head("Open issues"))
		for i, f := range st.LastSuccess.Findings {
			if i == 5 {
				u.dim(fmt.Sprintf("  … %d more — pgbot inspect for details", len(st.LastSuccess.Findings)-5))
				break
			}
			u.printf("%s %s\n", sevDot(u, f.Severity), f.Title)
		}
		u.println()
	}
	u.println(u.st.Dim("Commands:"))
	for _, c := range []string{"pgbot monitor now", "pgbot monitor edit", "pgbot monitor pause", "pgbot monitor stop"} {
		u.println(u.st.Dim("  " + c))
	}
	u.println()
	return nil
}

func healthWord(u *ui, r *monitor.RunRecord) string {
	crit, warn := r.Counts()
	score := fmt.Sprintf("%d/100", r.Score)
	switch {
	case crit > 0:
		return u.st.Crit(score + " · " + countOf(crit, "critical issue", "critical issues"))
	case warn > 0:
		return u.st.Warn(score + " · " + countOf(warn, "warning", "warnings"))
	case len(r.Incomplete) > 0:
		return score + u.st.Dim(" · partial data ("+r.Incomplete[0]+")")
	}
	return u.st.Good(score + " · Healthy")
}

// countOf renders "1 warning" / "3 warnings".
func countOf(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func activityIcon(u *ui, kind string) string {
	switch kind {
	case "ok":
		return u.st.Good("✓")
	case "warn":
		return u.st.Warn("⚠")
	case "crit", "fail":
		return u.st.Crit("✗")
	}
	return u.st.Dim("·")
}

func sevDot(u *ui, sev string) string {
	if sev == model.SeverityCritical {
		return u.st.Crit("●")
	}
	return u.st.Warn("●")
}

func (e *monitorEnv) nextCheck(cfg *monitor.Config, st *monitor.State) string {
	if cfg.Paused {
		return "Paused"
	}
	anchor := cfg.UpdatedAt
	if st != nil && st.LastAttempt.After(anchor) {
		anchor = st.LastAttempt
	}
	now := time.Now()
	next, approx := cfg.Schedule.Next(now, anchor)
	s := monitor.HumanWhen(next, now)
	if approx {
		s = "About " + lowerFirst(s)
	}
	return s
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func (e *monitorEnv) now(ctx context.Context) error {
	u := e.ui
	cfg, err := monitor.LoadConfig(e.paths.Config)
	if err != nil {
		return err
	}
	u.header("PgBot Monitor")
	var rep *monitor.Report
	err = u.spin("Checking "+cfg.Database.Name+"…", func() error {
		var err error
		rep, err = e.runner(cfg).Check(ctx, true)
		return err
	})
	if errors.Is(err, monitor.ErrBusy) {
		u.warn("A scheduled check is running right now — try again in a minute.")
		return nil
	}
	if err != nil {
		return err
	}
	e.printReport(cfg, rep)
	if rep.Err != nil {
		os.Exit(exitFailure)
	}
	return nil
}

// printReport renders one check for the terminal (and the launchd log).
func (e *monitorEnv) printReport(cfg *monitor.Config, rep *monitor.Report) {
	u := e.ui
	d := rep.Decision
	if rep.Err != nil {
		u.bad("Could not complete the check")
		u.dim("  " + rep.Err.Error())
		for _, l := range d.Summary[1:] {
			u.dim("  " + l)
		}
		u.dim("  The last good health result is kept; you'll be told when it's reachable again.")
	} else {
		run := d.State.LastSuccess
		u.ok("Connected to " + run.Database)
		u.ok("PostgreSQL diagnostics completed")
		u.ok("Compared with previous inspection")
		u.println()
		e.printHealthLines(run)
		emit := func(title string, paint func(string) string, fs []monitor.FindingState) {
			if len(fs) == 0 {
				return
			}
			u.println(paint(title))
			for _, f := range fs {
				u.printf("%s %s\n", paint("●"), f.Title)
			}
			u.println()
		}
		emit("NEW OR WORSE", u.st.Warn, append(append([]monitor.FindingState{}, d.Changes.Escalated...), d.Changes.New...))
		if len(d.Changes.Resolved) > 0 {
			emit("RESOLVED", u.st.Good, d.Changes.Resolved)
		}
		for _, l := range d.Summary {
			u.println(l)
		}
		u.println()
	}
	switch {
	case rep.Notified:
		u.ok("Notification requested " + u.st.Dim("(“"+d.Notify.Title+"”)"))
	case rep.NotifyErr != nil:
		u.warn("Notification failed: " + rep.NotifyErr.Error())
	default:
		u.dim("No notification — nothing changed that your settings (" + strings.ToLower(cfg.Policy.Describe()) + ") report.")
	}
	u.ok("Results saved")
	u.println()
}

func (e *monitorEnv) printHealthLines(r *monitor.RunRecord) {
	u := e.ui
	crit, warn := r.Counts()
	paint := u.st.Good
	switch {
	case crit > 0:
		paint = u.st.Crit
	case warn > 0:
		paint = u.st.Warn
	}
	u.println("Health score: " + paint(fmt.Sprintf("%d/100", r.Score)))
	switch {
	case crit > 0:
		u.println(u.st.Crit(countOf(crit, "critical issue", "critical issues")) + " needs attention.")
	case warn > 0:
		u.println("No critical issues. " + u.st.Warn(countOf(warn, "warning", "warnings")) + " open.")
	case len(r.Incomplete) > 0:
		u.println("No warnings found, but " + r.Incomplete[0] + ".")
	default:
		u.println("No critical issues detected.")
	}
	u.println()
}

func (e *monitorEnv) test() error {
	u := e.ui
	cfg, _ := monitor.LoadConfig(e.paths.Config) // test works before setup too
	name := "your database"
	sound := false
	if cfg != nil {
		name = cfg.Database.Name
		sound = cfg.Sound
	}
	n := monitor.Notification{
		Title: "🐘 PgBot · Test notification",
		Body:  "Notifications are working. PgBot will tell you when " + name + " needs attention.",
	}
	u.header("PgBot Monitor · Test")
	if err := e.plat.Notifier.Notify(n, sound); err != nil {
		u.bad("macOS did not accept the notification: " + err.Error())
		u.println()
		e.notificationHelp()
		return nil
	}
	u.ok("Notification requested — macOS accepted it.")
	u.dim("  macOS can't tell PgBot whether it was actually shown (permissions, Focus).")
	u.println()
	if !u.tty {
		e.notificationHelp()
		return nil
	}
	saw, err := u.confirm("Did it appear on your screen?", true)
	if err != nil {
		return nil
	}
	u.println()
	if saw {
		u.ok("Great — notifications are confirmed.")
	} else {
		e.notificationHelp()
	}
	if cfg != nil {
		cfg.NotificationsConfirmed = saw
		_ = monitor.SaveConfig(e.paths.Config, cfg)
	}
	return nil
}

func (e *monitorEnv) pause() error {
	cfg, err := monitor.LoadConfig(e.paths.Config)
	if err != nil {
		return err
	}
	if err := e.plat.Agents.Remove(e.paths.Label, e.paths.Plist); err != nil {
		return err
	}
	cfg.Paused = true
	cfg.UpdatedAt = time.Now()
	if err := monitor.SaveConfig(e.paths.Config, cfg); err != nil {
		return err
	}
	e.ui.ok("Monitoring paused. Your settings are kept — resume with `pgbot monitor resume`.")
	return nil
}

func (e *monitorEnv) resume() error {
	cfg, err := monitor.LoadConfig(e.paths.Config)
	if err != nil {
		return err
	}
	// Re-resolve the binary every time: this is also how a schedule is repaired
	// after pgbot moved or was upgraded to a new path.
	if p, err := durableExecutable(); err == nil {
		cfg.Program = p
	} else if _, serr := os.Stat(cfg.Program); serr != nil {
		return err
	}
	if _, err := e.plat.Secrets.Get(e.paths.KeychainSvc, monitor.KeychainAccount); err != nil {
		return fmt.Errorf("the saved connection can't be read (%v) — run `pgbot monitor edit`", err)
	}
	cfg.Paused = false
	cfg.UpdatedAt = time.Now()
	if err := monitor.SaveConfig(e.paths.Config, cfg); err != nil {
		return err
	}
	if err := e.install(cfg); err != nil {
		return err
	}
	st, _ := monitor.LoadState(e.paths.State)
	e.ui.ok("Monitoring resumed · " + cfg.Schedule.Describe())
	e.ui.dim("  Next check: " + e.nextCheck(cfg, st))
	return nil
}

// stop unregisters the job. Saved settings and the Keychain item are removed
// only with --purge or an explicit "yes"; baseline history is never touched.
func (e *monitorEnv) stop(purge, purgeSet bool) error {
	u := e.ui
	cfg, err := monitor.LoadConfig(e.paths.Config)
	if err != nil && !errors.Is(err, monitor.ErrNotConfigured) {
		u.warn(err.Error())
	}
	if err := e.plat.Agents.Remove(e.paths.Label, e.paths.Plist); err != nil {
		return err
	}
	u.ok("Scheduled monitoring stopped (LaunchAgent removed)")

	if !purgeSet && u.tty {
		u.println()
		u.println("Your settings and the saved connection in the Keychain are kept,")
		u.println("so `pgbot monitor resume` can restart monitoring later.")
		ans, err := u.confirm("Also delete the saved settings and Keychain connection?", false)
		if err == nil {
			purge = ans
		}
	}
	if !purge {
		if cfg != nil {
			cfg.Paused = true
			cfg.UpdatedAt = time.Now()
			_ = monitor.SaveConfig(e.paths.Config, cfg)
		}
		u.dim("  Settings kept. Remove them any time: pgbot monitor stop --purge")
		return nil
	}
	if err := e.plat.Secrets.Delete(e.paths.KeychainSvc, monitor.KeychainAccount); err != nil {
		u.warn("Could not delete the Keychain item: " + err.Error())
	} else {
		u.ok("Keychain connection deleted")
	}
	for _, p := range []string{e.paths.Config, e.paths.State, e.paths.Lock} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			u.warn(err.Error())
		}
	}
	if err := monitor.RemoveNotifierApp(e.paths.NotifierApp); err != nil {
		u.warn("Could not remove " + e.paths.NotifierApp + ": " + err.Error())
	}
	_ = os.Remove(e.paths.Dir)                       // only if empty
	_ = os.Remove(filepath.Dir(e.paths.NotifierApp)) // …/PgBot, only if empty
	u.ok("Monitor settings deleted")
	u.dim("  Baseline history (used by `pgbot inspect` / `diff`) was left untouched.")
	return nil
}

// scheduledRun is `pgbot monitor _run`, started by launchd. It never prompts,
// writes plain text to the log, and always exits 0 for an ordinary failed
// check (the failure is recorded and notified, not a launchd problem).
func (e *monitorEnv) scheduledRun(ctx context.Context) error {
	_ = monitor.TrimLog(e.paths.Log, 1<<20, 256<<10)
	e.ui.color, e.ui.st, e.ui.tty = false, render.NewStyler(false), false
	stamp := time.Now().Format(time.RFC3339)
	cfg, err := monitor.LoadConfig(e.paths.Config)
	if err != nil {
		fmt.Printf("%s pgbot monitor: %s\n", stamp, monitor.Redact(err.Error()))
		return nil
	}
	if cfg.Paused {
		fmt.Printf("%s pgbot monitor: paused — skipping\n", stamp)
		return nil
	}
	fmt.Printf("%s pgbot monitor: checking %s (pgbot %s)\n", stamp, cfg.Database.Name, version)
	rep, err := e.runner(cfg).Check(ctx, false)
	if errors.Is(err, monitor.ErrBusy) {
		fmt.Printf("%s pgbot monitor: another check is running — skipping\n", stamp)
		return nil
	}
	if err != nil {
		fmt.Printf("%s pgbot monitor: %s\n", stamp, monitor.Redact(err.Error()))
		return nil
	}
	e.printReport(cfg, rep)
	return nil
}

// durableExecutable resolves the pgbot binary the LaunchAgent should run, and
// refuses one that may disappear (npx cache, `go run`, temp dirs). When the
// PATH entry is a stable symlink to this binary (Homebrew's /opt/homebrew/bin),
// that path is preferred so `brew upgrade` doesn't strand the schedule.
func durableExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		real = exe
	}
	if why := monitor.EphemeralReason(real); why != "" {
		return "", fmt.Errorf("this pgbot can't be scheduled because %s.\n"+
			"Install pgbot permanently, then run `pgbot monitor` again:\n"+
			"  brew install pgrundev/tap/pgbot\n"+
			"  npm i -g @pgbot/cli\n"+
			"  curl -fsSL https://pgbot.dev/install | sh", why)
	}
	if p, err := exec.LookPath("pgbot"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			if rp, err := filepath.EvalSymlinks(abs); err == nil && rp == real && monitor.EphemeralReason(abs) == "" {
				return abs, nil
			}
		}
	}
	return real, nil
}
