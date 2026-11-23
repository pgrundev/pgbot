package monitor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

func validConfig() *Config {
	return &Config{
		Version:  configVersion,
		Database: Database{Name: "production", Host: "db.example.com"},
		Schedule: DefaultSchedule(),
		Policy:   PolicyImportant,
		Program:  "/opt/homebrew/bin/pgbot",
	}
}

func TestConfigRoundTripAndPermissions(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "config.json")
	c := validConfig()
	if err := SaveConfig(p, c); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode %v, want 0600", fi.Mode().Perm())
	}
	got, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Database.Name != "production" || got.Schedule != c.Schedule || got.Policy != PolicyImportant {
		t.Errorf("round trip: %+v", got)
	}
	// No leftover temp files from the atomic write.
	ents, _ := os.ReadDir(filepath.Dir(p))
	if len(ents) != 1 {
		t.Errorf("atomic write left files behind: %v", ents)
	}
}

func TestConfigMissingAndInvalid(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadConfig(filepath.Join(dir, "nope.json")); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("missing: %v", err)
	}
	p := filepath.Join(dir, "config.json")
	os.WriteFile(p, []byte("{not json"), 0o600)
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("corrupt: %v", err)
	}
	bad := []func(*Config){
		func(c *Config) { c.Policy = "sometimes" },
		func(c *Config) { c.Schedule = Schedule{Kind: "monthly"} },
		func(c *Config) { c.Database.Name = "" },
		func(c *Config) { c.Program = "pgbot" },
		func(c *Config) { c.Version = 99 },
	}
	for i, mut := range bad {
		c := validConfig()
		mut(c)
		if err := SaveConfig(p, c); err == nil {
			t.Errorf("case %d: SaveConfig accepted an invalid config", i)
		}
		writeJSONAtomic(p, c)
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("case %d: LoadConfig accepted an invalid config", i)
		}
	}
}

func TestConfigHoldsNoSecret(t *testing.T) {
	t.Setenv("PGPASSWORD", "hunter2")
	t.Setenv("DATABASE_URL", "postgres://u:hunter2@h/db")
	t.Setenv("PGPASSFILE", "/Users/me/.pgpass")
	env := CaptureEnv()
	if _, ok := env["PGPASSWORD"]; ok {
		t.Fatal("PGPASSWORD captured")
	}
	if _, ok := env["DATABASE_URL"]; ok {
		t.Fatal("DATABASE_URL captured")
	}
	if env["PGPASSFILE"] != "/Users/me/.pgpass" {
		t.Errorf("PGPASSFILE not captured: %v", env)
	}
}

func TestStatePersistence(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	s, err := LoadState(p)
	if err != nil || s.LastSuccess != nil {
		t.Fatalf("fresh state: %+v %v", s, err)
	}
	s.LastSuccess = run(t0, 90, slow)
	s.Failure = &ConnFailure{Since: t0, Count: 2, Notified: true}
	if err := SaveState(p, s); err != nil {
		t.Fatal(err)
	}
	back, err := LoadState(p)
	if err != nil || back.LastSuccess.Score != 90 || back.Failure.Count != 2 || back.LastSuccess.Findings[0].Key() != slow.Key() {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	os.WriteFile(p, []byte("garbage"), 0o600)
	fresh, err := LoadState(p)
	if err == nil || fresh == nil || fresh.LastSuccess != nil {
		t.Errorf("corrupt state should report an error and start fresh: %+v %v", fresh, err)
	}
}

func TestLockExcludesConcurrentRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("monitor is macOS-only")
	}
	p := filepath.Join(t.TempDir(), "monitor.lock")
	unlock, err := Lock(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(p); !errors.Is(err, ErrBusy) {
		t.Fatalf("second lock: %v, want ErrBusy", err)
	}
	unlock()
	u2, err := Lock(p)
	if err != nil {
		t.Fatalf("relock after unlock: %v", err)
	}
	u2()
}

func TestConcurrentSaveStateNeverCorrupts(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := &State{LastSuccess: run(t0, i, slow)}
			_ = SaveState(p, s)
		}(i)
	}
	wg.Wait()
	if _, err := LoadState(p); err != nil {
		t.Fatalf("state corrupted by concurrent atomic writes: %v", err)
	}
}

func TestRedact(t *testing.T) {
	cases := []string{
		"dial postgres://app:hunter2@db.example.com:5432/prod?sslmode=require failed",
		"postgresql://app:hunter2@db/prod",
		"host=db user=app password=hunter2 dbname=prod",
		"host=db password='hunter 2' dbname=prod",
		"postgres://h/db?password=hunter2",
	}
	for _, in := range cases {
		out := Redact(in)
		if strings.Contains(out, "hunter") {
			t.Errorf("Redact(%q) = %q leaks the secret", in, out)
		}
	}
	if got := Redact("line1\nline2\x07"); got != "line1 line2" {
		t.Errorf("control chars: %q", got)
	}
	if got := clip("short", 10); got != "short" {
		t.Errorf("clip short: %q", got)
	}
	if got := clip("alpha beta gamma delta epsilon", 18); len([]rune(got)) > 18 || !strings.HasSuffix(got, "…") {
		t.Errorf("clip: %q", got)
	}
}

func TestPlistCalendarAndInterval(t *testing.T) {
	spec := AgentSpec{Label: Label, Program: "/opt/homebrew/bin/pgbot", Log: "/Users/me/Library/Logs/PgBot/monitor.log",
		Env: map[string]string{"PGPASSFILE": "/Users/me/.pgpass"}}

	spec.Schedule = Schedule{Kind: Weekly, Weekday: time.Monday, Hour: 9, Minute: 5}
	b, err := Plist(spec)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"<key>Label</key>", "<string>dev.pgbot.monitor</string>",
		"<string>/opt/homebrew/bin/pgbot</string>", "<string>monitor</string>", "<string>_run</string>",
		"<key>StartCalendarInterval</key>", "<key>Weekday</key>\n\t\t<integer>1</integer>",
		"<key>Hour</key>\n\t\t<integer>9</integer>", "<key>Minute</key>\n\t\t<integer>5</integer>",
		"<key>RunAtLoad</key>\n\t<false/>", "<string>Background</string>", "<key>PGPASSFILE</key>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("weekly plist lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "StartInterval</key>") {
		t.Error("weekly plist has StartInterval")
	}
	if ProgramFromPlist(b) != "/opt/homebrew/bin/pgbot" {
		t.Errorf("ProgramFromPlist = %q", ProgramFromPlist(b))
	}

	spec.Schedule = Schedule{Kind: Daily, Hour: 21}
	b, _ = Plist(spec)
	if strings.Contains(string(b), "<key>Weekday</key>") {
		t.Error("daily plist has a Weekday")
	}

	spec.Schedule = Schedule{Kind: Every6h}
	b, _ = Plist(spec)
	if !strings.Contains(string(b), "<key>StartInterval</key>\n\t<integer>21600</integer>") || strings.Contains(string(b), "StartCalendarInterval") {
		t.Errorf("6h plist:\n%s", b)
	}
	spec.Schedule = Schedule{Kind: Hourly}
	b, _ = Plist(spec)
	if !strings.Contains(string(b), "<integer>3600</integer>") {
		t.Errorf("hourly plist:\n%s", b)
	}

	// XML-special characters in paths are escaped; a relative program is refused.
	spec.Program = "/Users/a&b/<bin>/pgbot"
	b, _ = Plist(spec)
	if !strings.Contains(string(b), "/Users/a&amp;b/&lt;bin&gt;/pgbot") || ProgramFromPlist(b) != spec.Program {
		t.Errorf("escaping: %s / %q", b, ProgramFromPlist(b))
	}
	spec.Program = "pgbot"
	if _, err := Plist(spec); err == nil {
		t.Error("relative program accepted")
	}

	if runtime.GOOS == "darwin" {
		spec.Program = "/opt/homebrew/bin/pgbot"
		spec.Schedule = Schedule{Kind: Weekly, Weekday: time.Sunday, Hour: 23, Minute: 59}
		b, _ = Plist(spec)
		p := filepath.Join(t.TempDir(), "x.plist")
		os.WriteFile(p, b, 0o644)
		if out, err := exec.Command("/usr/bin/plutil", "-lint", p).CombinedOutput(); err != nil {
			t.Fatalf("plutil rejected the plist: %s\n%s", out, b)
		}
	}
}

func TestEphemeralReason(t *testing.T) {
	bad := []string{
		"/Users/me/.npm/_npx/1a2b/node_modules/@pgbot/darwin-arm64/bin/pgbot",
		"/var/folders/xy/T/go-build123/b001/exe/pgbot",
		"/private/var/folders/xy/T/pgbot",
		"/tmp/pgbot",
		filepath.Join(os.TempDir(), "pgbot"),
	}
	for _, p := range bad {
		if EphemeralReason(p) == "" {
			t.Errorf("%s should be ephemeral", p)
		}
	}
	good := []string{
		"/opt/homebrew/bin/pgbot", "/usr/local/bin/pgbot", "/Users/me/.local/bin/pgbot",
		"/opt/homebrew/lib/node_modules/@pgbot/darwin-arm64/bin/pgbot",
	}
	for _, p := range good {
		if why := EphemeralReason(p); why != "" {
			t.Errorf("%s flagged: %s", p, why)
		}
	}
}

func TestTrimLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "monitor.log")
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		b.WriteString("2026-11-18 a log line that is about sixty characters long....\n")
	}
	os.WriteFile(p, []byte(b.String()), 0o600)
	if err := TrimLog(p, 10_000, 2_000); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if len(data) > 2_100 || !strings.HasPrefix(string(data), "… (older") || !strings.HasSuffix(string(data), "....\n") {
		t.Errorf("trimmed log: %d bytes, starts %q", len(data), string(data[:20]))
	}
	if err := TrimLog(filepath.Join(t.TempDir(), "absent"), 1, 1); err != nil {
		t.Errorf("absent log: %v", err)
	}
}

// ---- runner with fakes ------------------------------------------------------

type fakeSecrets struct {
	m   map[string]string
	err error
}

func (f *fakeSecrets) Set(s, a, v string) error { f.m[s+a] = v; return nil }
func (f *fakeSecrets) Get(s, a string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	v, ok := f.m[s+a]
	if !ok {
		return "", ErrNoSecret
	}
	return v, nil
}
func (f *fakeSecrets) Delete(s, a string) error { delete(f.m, s+a); return nil }

type fakeNotifier struct{ sent []Notification }

func (f *fakeNotifier) Notify(n Notification, _ bool) error { f.sent = append(f.sent, n); return nil }

func testRunner(t *testing.T, inspect Inspector) (Runner, *fakeSecrets, *fakeNotifier) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("monitor is macOS-only")
	}
	dir := t.TempDir()
	t.Setenv("PGBOT_MONITOR_HOME", dir)
	t.Setenv("PGBOT_MONITOR_LABEL", "dev.pgbot.monitor.test")
	paths, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(paths.Config, validConfig()); err != nil {
		t.Fatal(err)
	}
	sec := &fakeSecrets{m: map[string]string{paths.KeychainSvc + KeychainAccount: "postgres://u:pw@h/production"}}
	n := &fakeNotifier{}
	clock := t0
	return Runner{Paths: paths, Platform: Platform{Secrets: sec, Notifier: n}, Inspect: inspect,
		Now: func() time.Time { clock = clock.Add(time.Hour); return clock }}, sec, n
}

func ctxWith(findings ...model.Finding) *model.Context {
	return &model.Context{CollectedAt: t0, Fingerprint: "fp1", Health: &model.Health{},
		Server: model.ServerInfo{Database: "production", VersionNum: 180001}, Findings: findings}
}

func TestRunnerEndToEnd(t *testing.T) {
	var current *model.Context
	var failWith error
	var gotConn string
	r, _, n := testRunner(t, func(_ context.Context, cs string) (*InspectResult, error) {
		gotConn = cs
		if failWith != nil {
			return nil, failWith
		}
		return &InspectResult{Context: current, Score: 100 - 3*len(current.Findings), HasPgMonitor: true}, nil
	})

	current = ctxWith()
	rep, err := r.Check(context.Background(), false)
	if err != nil || rep.Err != nil || !rep.Notified || !strings.Contains(n.sent[0].Title, "healthy") {
		t.Fatalf("first run: %+v %v %+v", rep, err, n.sent)
	}
	if gotConn != "postgres://u:pw@h/production" {
		t.Errorf("inspector got %q", gotConn)
	}

	current = ctxWith(model.Finding{ID: "slow_queries", Object: "q:1", Severity: model.SeverityWarn, Title: "Queries are 3.2× slower"})
	rep, _ = r.Check(context.Background(), false)
	if !rep.Notified || len(n.sent) != 2 || n.sent[1].Kind != "warn" {
		t.Fatalf("new warning: %+v", n.sent)
	}
	rep, _ = r.Check(context.Background(), false)
	if rep.Notified || len(n.sent) != 2 {
		t.Fatalf("repeat warning notified: %+v", n.sent)
	}

	failWith = errors.New("dial tcp: connection refused (postgres://u:pw@h/production)")
	rep, _ = r.Check(context.Background(), false)
	if rep.Err == nil || strings.Contains(rep.Err.Error(), ":pw@") || len(n.sent) != 3 {
		t.Fatalf("failure: %+v %+v", rep.Err, n.sent)
	}
	st, _ := LoadState(r.Paths.State)
	if st.LastSuccess == nil || len(st.LastSuccess.Findings) != 1 || st.Failure == nil {
		t.Fatalf("persisted state after failure: %+v", st)
	}
	data, _ := os.ReadFile(r.Paths.State)
	if strings.Contains(string(data), ":pw@") {
		t.Fatal("state.json contains the password")
	}

	failWith = nil
	current = ctxWith()
	rep, _ = r.Check(context.Background(), false)
	if !rep.Notified || !strings.Contains(n.sent[3].Title, "Connection restored") {
		t.Fatalf("restore: %+v", n.sent)
	}
}

func TestRunnerMissingSecretIsAFailedCheck(t *testing.T) {
	r, sec, n := testRunner(t, func(context.Context, string) (*InspectResult, error) {
		t.Fatal("inspector must not run without a connection")
		return nil, nil
	})
	delete(sec.m, r.Paths.KeychainSvc+KeychainAccount)
	rep, err := r.Check(context.Background(), false)
	if err != nil || rep.Err == nil || !strings.Contains(rep.Err.Error(), "Keychain") || len(n.sent) != 1 {
		t.Fatalf("missing secret: %+v %v %+v", rep, err, n.sent)
	}
}

func TestRunnerBusy(t *testing.T) {
	r, _, _ := testRunner(t, func(context.Context, string) (*InspectResult, error) {
		return &InspectResult{Context: ctxWith(), Score: 100, HasPgMonitor: true}, nil
	})
	unlock, err := Lock(r.Paths.Lock)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := r.Check(context.Background(), false); !errors.Is(err, ErrBusy) {
		t.Fatalf("overlapping run: %v", err)
	}
}

func TestRunnerNotConfigured(t *testing.T) {
	r, _, _ := testRunner(t, nil)
	os.Remove(r.Paths.Config)
	if _, err := r.Check(context.Background(), false); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("got %v", err)
	}
}
