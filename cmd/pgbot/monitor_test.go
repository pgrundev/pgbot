package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/monitor"
	"github.com/pgrundev/pgbot/internal/render"
)

type fakeAgents struct {
	loaded   map[string]bool
	installs int
	lastPl   []byte
}

func (f *fakeAgents) Install(label, path string, pl []byte) error {
	if err := os.WriteFile(path, pl, 0o644); err != nil {
		return err
	}
	f.loaded[label] = true // replaces, never duplicates
	f.installs++
	f.lastPl = pl
	return nil
}
func (f *fakeAgents) Remove(label, path string) error {
	delete(f.loaded, label)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
func (f *fakeAgents) Loaded(label string) bool { return f.loaded[label] }
func (f *fakeAgents) Kick(string) error        { return nil }

type memSecrets map[string]string

func (m memSecrets) Set(s, a, v string) error { m[s+"/"+a] = v; return nil }
func (m memSecrets) Get(s, a string) (string, error) {
	v, ok := m[s+"/"+a]
	if !ok {
		return "", monitor.ErrNoSecret
	}
	return v, nil
}
func (m memSecrets) Delete(s, a string) error { delete(m, s+"/"+a); return nil }

type sinkNotifier struct{ n []monitor.Notification }

func (s *sinkNotifier) Notify(n monitor.Notification, _ bool) error { s.n = append(s.n, n); return nil }

// testMonitorEnv builds a non-interactive env over temp paths and fakes.
func testMonitorEnv(t *testing.T) (*monitorEnv, *fakeAgents, memSecrets, *bytes.Buffer) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("monitor is macOS-only")
	}
	dir := t.TempDir()
	t.Setenv("PGBOT_MONITOR_HOME", dir)
	t.Setenv("PGBOT_MONITOR_AGENT_DIR", filepath.Join(dir, "agents"))
	t.Setenv("PGBOT_MONITOR_LABEL", "dev.pgbot.monitor.test")
	paths, err := monitor.DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(paths.AgentDir, 0o755)
	ag := &fakeAgents{loaded: map[string]bool{}}
	sec := memSecrets{}
	out := &bytes.Buffer{}
	u := &ui{in: os.Stdin, r: bufio.NewReader(strings.NewReader("")), out: out, st: render.NewStyler(false)}
	e := &monitorEnv{paths: paths, plat: monitor.Platform{Secrets: sec, Agents: ag, Notifier: &sinkNotifier{}}, ui: u}

	prog := filepath.Join(dir, "pgbot")
	os.WriteFile(prog, []byte("#!/bin/sh\n"), 0o755)
	cfg := &monitor.Config{
		Database: monitor.Database{Name: "production"}, Schedule: monitor.DefaultSchedule(),
		Policy: monitor.PolicyImportant, Program: prog, UpdatedAt: time.Now(),
	}
	if err := monitor.SaveConfig(paths.Config, cfg); err != nil {
		t.Fatal(err)
	}
	sec.Set(paths.KeychainSvc, monitor.KeychainAccount, "postgres://u:pw@h/production")
	if err := e.install(cfg); err != nil {
		t.Fatal(err)
	}
	return e, ag, sec, out
}

func TestMonitorPauseResume(t *testing.T) {
	e, ag, _, out := testMonitorEnv(t)
	if err := e.pause(); err != nil {
		t.Fatal(err)
	}
	cfg, _ := monitor.LoadConfig(e.paths.Config)
	if !cfg.Paused || ag.Loaded(e.paths.Label) {
		t.Fatalf("pause: paused=%v loaded=%v", cfg.Paused, ag.Loaded(e.paths.Label))
	}
	if _, err := os.Stat(e.paths.Plist); !os.IsNotExist(err) {
		t.Fatal("pause left the plist in LaunchAgents (it would reload at next login)")
	}
	if err := e.resume(); err != nil {
		t.Fatal(err)
	}
	cfg, _ = monitor.LoadConfig(e.paths.Config)
	if cfg.Paused || !ag.Loaded(e.paths.Label) {
		t.Fatalf("resume: paused=%v loaded=%v", cfg.Paused, ag.Loaded(e.paths.Label))
	}
	if cfg.Schedule != monitor.DefaultSchedule() {
		t.Errorf("resume changed the schedule: %+v", cfg.Schedule)
	}
	if !strings.Contains(out.String(), "Monitoring resumed · Every day at 9:00 AM") {
		t.Errorf("output: %s", out)
	}
	if bytes.Contains(ag.lastPl, []byte("pw@")) || bytes.Contains(ag.lastPl, []byte("postgres://")) {
		t.Fatal("plist contains the connection")
	}
}

func TestMonitorResumeNeedsSecret(t *testing.T) {
	e, _, sec, _ := testMonitorEnv(t)
	e.pause()
	sec.Delete(e.paths.KeychainSvc, monitor.KeychainAccount)
	if err := e.resume(); err == nil || !strings.Contains(err.Error(), "monitor edit") {
		t.Fatalf("resume without a secret: %v", err)
	}
}

func TestMonitorStopKeepsSettingsByDefault(t *testing.T) {
	e, ag, sec, _ := testMonitorEnv(t)
	if err := e.stop(false, false); err != nil { // non-interactive: no prompt, keep
		t.Fatal(err)
	}
	if ag.Loaded(e.paths.Label) {
		t.Fatal("job still loaded after stop")
	}
	cfg, err := monitor.LoadConfig(e.paths.Config)
	if err != nil || !cfg.Paused {
		t.Fatalf("settings should be kept (paused): %v", err)
	}
	if _, err := sec.Get(e.paths.KeychainSvc, monitor.KeychainAccount); err != nil {
		t.Fatal("keychain item deleted without --purge")
	}
}

func TestMonitorStopPurge(t *testing.T) {
	e, _, sec, _ := testMonitorEnv(t)
	baseline := filepath.Join(t.TempDir(), "baselines.db")
	os.WriteFile(baseline, []byte("history"), 0o600)
	if err := e.stop(true, true); err != nil {
		t.Fatal(err)
	}
	if _, err := monitor.LoadConfig(e.paths.Config); !errors.Is(err, monitor.ErrNotConfigured) {
		t.Fatalf("config survived purge: %v", err)
	}
	if _, err := sec.Get(e.paths.KeychainSvc, monitor.KeychainAccount); !errors.Is(err, monitor.ErrNoSecret) {
		t.Fatal("keychain item survived purge")
	}
	if _, err := os.Stat(baseline); err != nil {
		t.Fatal("purge touched unrelated files")
	}
}

func TestMonitorEditReinstallDoesNotDuplicate(t *testing.T) {
	e, ag, _, _ := testMonitorEnv(t)
	cfg, _ := monitor.LoadConfig(e.paths.Config)
	cfg.Schedule = monitor.Schedule{Kind: monitor.Weekly, Weekday: time.Friday, Hour: 17}
	if err := e.install(cfg); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(e.paths.AgentDir)
	if len(ents) != 1 || len(ag.loaded) != 1 {
		t.Fatalf("expected exactly one agent, got files=%d loaded=%d", len(ents), len(ag.loaded))
	}
	if !bytes.Contains(ag.lastPl, []byte("<key>Weekday</key>")) {
		t.Error("new schedule not written")
	}
}

func TestMonitorStatusNotYetChecked(t *testing.T) {
	e, _, _, out := testMonitorEnv(t)
	if err := e.status(); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"● Active", "production", "Every day at 9:00 AM", "Not yet checked", "Next check"} {
		if !strings.Contains(s, want) {
			t.Errorf("status lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "Healthy") || strings.Contains(s, "/100") {
		t.Errorf("status invented a health result before any check:\n%s", s)
	}
}

func TestMonitorStatusFlagsMissingBinary(t *testing.T) {
	e, _, _, out := testMonitorEnv(t)
	cfg, _ := monitor.LoadConfig(e.paths.Config)
	os.Remove(cfg.Program)
	e.status()
	if !strings.Contains(out.String(), "scheduled pgbot binary is missing") {
		t.Errorf("missing binary not reported:\n%s", out)
	}
}

func TestMonitorWizardNeedsTTY(t *testing.T) {
	e, _, _, _ := testMonitorEnv(t)
	err := e.wizard(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "interactive") {
		t.Fatalf("wizard without a TTY: %v", err)
	}
}

func TestMonitorScheduledRunWhilePausedIsQuiet(t *testing.T) {
	e, _, _, _ := testMonitorEnv(t)
	e.pause()
	if err := e.scheduledRun(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, _ := monitor.LoadState(e.paths.State); !st.LastAttempt.IsZero() {
		t.Fatal("a paused monitor ran a check")
	}
}

func TestMonitorCommandTree(t *testing.T) {
	cmd := newMonitorCmd()
	want := map[string]bool{"status": false, "now": false, "test": false, "edit": false, "pause": false, "resume": false, "stop": false, "_run": false}
	for _, c := range cmd.Commands() {
		if _, ok := want[c.Name()]; ok {
			want[c.Name()] = true
		}
		if c.Name() == "_run" && !c.Hidden {
			t.Error("_run must be hidden")
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("missing subcommand %q", k)
		}
	}
}

func TestMonitorUnsupportedOS(t *testing.T) {
	_, err := newMonitorEnv()
	if runtime.GOOS == "darwin" {
		if err != nil {
			t.Fatalf("darwin: %v", err)
		}
		return
	}
	if !errors.Is(err, monitor.ErrUnsupported) || err.Error() != "PgBot Monitor currently supports macOS only" {
		t.Fatalf("non-darwin: %v", err)
	}
}
