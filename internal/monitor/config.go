package monitor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Label is the launchd job label and the Keychain service name. Tests and
// side-by-side experiments can override it (and the directories) through
// PGBOT_MONITOR_LABEL / PGBOT_MONITOR_HOME so they never touch a real setup.
const Label = "dev.pgbot.monitor"

// Policy decides which run outcomes become notifications.
type Policy string

const (
	PolicyImportant Policy = "important" // new/escalated warnings+criticals, recoveries, connectivity (default)
	PolicyEvery     Policy = "every"     // a summary after every check, plus failures
	PolicyCritical  Policy = "critical"  // new/escalated criticals, connectivity, and recovery from those
)

// Describe is the short label used in the UI.
func (p Policy) Describe() string {
	switch p {
	case PolicyImportant:
		return "Important changes"
	case PolicyEvery:
		return "Every health check"
	case PolicyCritical:
		return "Critical issues only"
	}
	return string(p)
}

func (p Policy) valid() bool {
	return p == PolicyImportant || p == PolicyEvery || p == PolicyCritical
}

// Database is the NON-SECRET identity of the monitored database, for display
// and for matching state across runs. The connection string itself (which may
// carry a password) lives only in the Keychain.
type Database struct {
	Name        string `json:"name"`                  // current_database()
	Host        string `json:"host,omitempty"`        // display only
	Port        int    `json:"port,omitempty"`        // display only
	User        string `json:"user,omitempty"`        // display only
	Fingerprint string `json:"fingerprint,omitempty"` // same key the baseline store uses
	Version     string `json:"version,omitempty"`     // e.g. "18.1", from the last successful connection
}

// Config is config.json: preferences plus non-secret connection metadata.
type Config struct {
	Version  int      `json:"version"`
	Paused   bool     `json:"paused"`
	Database Database `json:"database"`
	Schedule Schedule `json:"schedule"`
	Policy   Policy   `json:"policy"`
	// Sound plays the default notification sound; quiet by default.
	Sound bool `json:"sound,omitempty"`
	// NotificationsConfirmed is true only once the user said they SAW a test
	// notification — never inferred from osascript exiting 0.
	NotificationsConfirmed bool `json:"notifications_confirmed,omitempty"`
	// Program is the durable pgbot path written into the LaunchAgent.
	Program string `json:"program"`
	// PgbotConfig is the .pgbot.toml the setup run discovered (absolute), so
	// scheduled runs — which start in / — apply the same thresholds and ignores.
	PgbotConfig string `json:"pgbot_config,omitempty"`
	// Env carries non-secret libpq/XDG file locations (PGSERVICEFILE,
	// PGPASSFILE, …) captured at setup; launchd starts with an empty
	// environment, and these decide where .pgpass and the baseline store live.
	Env       map[string]string `json:"env,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// PassthroughEnv lists the variables copied into Config.Env. None of them may
// hold a secret — PGPASSWORD and DATABASE_URL are deliberately absent.
var PassthroughEnv = []string{
	"PGSERVICEFILE", "PGPASSFILE", "PGSYSCONFDIR", "PGSSLROOTCERT", "PGSSLCERT", "PGSSLKEY",
	"XDG_STATE_HOME", "XDG_CONFIG_HOME", "PGBOT_SSH_TUNNEL",
	// Test isolation: a side-by-side monitor must stay side-by-side when launchd runs it.
	"PGBOT_MONITOR_HOME", "PGBOT_MONITOR_LABEL", "PGBOT_MONITOR_AGENT_DIR",
}

const configVersion = 1

// Validate checks a loaded or about-to-be-saved config.
func (c *Config) Validate() error {
	if c.Version != configVersion {
		return fmt.Errorf("unsupported monitor config version %d", c.Version)
	}
	if err := c.Schedule.Validate(); err != nil {
		return err
	}
	if !c.Policy.valid() {
		return fmt.Errorf("unsupported notification policy %q", c.Policy)
	}
	if c.Database.Name == "" {
		return fmt.Errorf("database name is missing")
	}
	if c.Program == "" || !filepath.IsAbs(c.Program) {
		return fmt.Errorf("pgbot executable path is missing or relative")
	}
	return nil
}

// Paths are the monitor's files on disk.
type Paths struct {
	Dir         string // config + state
	Config      string
	State       string
	Lock        string
	LogDir      string
	Log         string
	AgentDir    string
	Plist       string
	Label       string
	KeychainSvc string
	NotifierApp string // PgBot.app, the helper that posts banners with PgBot's name and icon
}

// DefaultPaths resolves the standard macOS user locations, honoring the
// PGBOT_MONITOR_HOME / PGBOT_MONITOR_LABEL test overrides. Under an override
// everything (including the LaunchAgent plist) stays inside that directory
// except that the plist still goes to ~/Library/LaunchAgents when
// PGBOT_MONITOR_AGENT_DIR is unset — launchd only reads plists by path, so a
// test agent can live anywhere.
func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	label := Label
	if l := os.Getenv("PGBOT_MONITOR_LABEL"); l != "" {
		label = l
	}
	p := Paths{
		Dir:      filepath.Join(home, "Library", "Application Support", "PgBot", "monitor"),
		LogDir:   filepath.Join(home, "Library", "Logs", "PgBot"),
		AgentDir: filepath.Join(home, "Library", "LaunchAgents"),
		Label:    label,
	}
	p.NotifierApp = filepath.Join(filepath.Dir(p.Dir), "PgBot.app") // ~/Library/Application Support/PgBot/PgBot.app
	if h := os.Getenv("PGBOT_MONITOR_HOME"); h != "" {
		p.Dir = h
		p.LogDir = h
		p.NotifierApp = filepath.Join(h, "PgBot.app")
	}
	if a := os.Getenv("PGBOT_MONITOR_AGENT_DIR"); a != "" {
		p.AgentDir = a
	}
	p.Config = filepath.Join(p.Dir, "config.json")
	p.State = filepath.Join(p.Dir, "state.json")
	p.Lock = filepath.Join(p.Dir, "monitor.lock")
	p.Log = filepath.Join(p.LogDir, "monitor.log")
	p.Plist = filepath.Join(p.AgentDir, label+".plist")
	p.KeychainSvc = label
	return p, nil
}

// ErrNotConfigured means there is no config.json yet.
var ErrNotConfigured = errors.New("PgBot Monitor is not set up yet — run `pgbot monitor`")

// LoadConfig reads and validates config.json.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotConfigured
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("monitor config %s is corrupt: %w (re-run `pgbot monitor edit`)", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("monitor config %s is invalid: %w (re-run `pgbot monitor edit`)", path, err)
	}
	return &c, nil
}

// SaveConfig validates and atomically writes config.json (0600).
func SaveConfig(path string, c *Config) error {
	if c.Version == 0 {
		c.Version = configVersion
	}
	if err := c.Validate(); err != nil {
		return err
	}
	return writeJSONAtomic(path, c)
}

// writeJSONAtomic writes v to path via a same-directory temp file + fsync +
// rename, so a crash or a concurrent reader never sees a half-written file.
// The directory is created 0700 and the file 0600.
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
