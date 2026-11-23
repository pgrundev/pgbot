package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

// InspectResult is what one run of pgbot's own inspection pipeline yields.
type InspectResult struct {
	Context      *model.Context
	Score        int  // render.HealthScore — the same number `pgbot inspect` prints
	HasPgMonitor bool // from the connection's capability probe
}

// Inspector runs pgbot's inspection pipeline (connect → collect → baseline →
// findings) against connString. The CLI supplies the real one; tests a fake.
type Inspector func(ctx context.Context, connString string) (*InspectResult, error)

// Runner performs one monitor check.
type Runner struct {
	Paths    Paths
	Platform Platform
	Inspect  Inspector
	Now      func() time.Time
	Timeout  time.Duration // whole-check budget; default 2m
}

// Report is what a check did, for the terminal and the log.
type Report struct {
	Config    *Config
	Decision  Decision
	Err       error // the check's failure, if any (redacted)
	Notified  bool  // macOS accepted a banner
	NotifyErr error
}

// Check runs one health check end to end: lock, load settings + secret,
// inspect, compare with the previous successful run, persist, notify.
func (r Runner) Check(ctx context.Context, manual bool) (*Report, error) {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	unlock, err := Lock(r.Paths.Lock)
	if err != nil {
		return nil, err
	}
	defer unlock()

	cfg, err := LoadConfig(r.Paths.Config)
	if err != nil {
		return nil, err
	}
	ApplyEnv(cfg.Env)

	prev, stateErr := LoadState(r.Paths.State)
	if prev == nil {
		return nil, stateErr
	}

	var res *InspectResult
	secret, err := r.Platform.Secrets.Get(r.Paths.KeychainSvc, KeychainAccount)
	switch {
	case errors.Is(err, ErrNoSecret):
		err = errors.New("the saved connection is missing from the Keychain — run `pgbot monitor edit`")
	case err == nil:
		timeout := r.Timeout
		if timeout == 0 {
			timeout = 2 * time.Minute
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		res, err = r.Inspect(cctx, secret)
		cancel()
	}

	out := Outcome{At: now(), Manual: manual}
	if err != nil {
		out.Err = errors.New(Redact(err.Error()))
	} else {
		run := Snapshot(res.Context, res.Score, Incomplete(res.Context, res.HasPgMonitor))
		run.At = out.At
		out.Run = run
	}

	d := Decide(cfg.Policy, cfg.Database.Name, prev, out)
	if stateErr != nil {
		d.Summary = append(d.Summary, stateErr.Error())
	}
	if err := SaveState(r.Paths.State, d.State); err != nil {
		return nil, fmt.Errorf("save monitor state: %w", err)
	}
	if out.Run != nil && out.Run.Version != "" && (cfg.Database.Version != out.Run.Version || cfg.Database.Fingerprint == "") {
		cfg.Database.Version = out.Run.Version
		if cfg.Database.Fingerprint == "" {
			cfg.Database.Fingerprint = out.Run.Fingerprint
		}
		cfg.UpdatedAt = out.At
		_ = SaveConfig(r.Paths.Config, cfg) // display metadata only; never fatal
	}

	rep := &Report{Config: cfg, Decision: d, Err: out.Err}
	if d.Notify != nil {
		if nerr := r.Platform.Notifier.Notify(*d.Notify, cfg.Sound); nerr != nil {
			rep.NotifyErr = nerr
		} else {
			rep.Notified = true
		}
	}
	return rep, nil
}

// ApplyEnv sets the saved non-secret environment for variables the current
// process doesn't already have (launchd's plist sets them too; this keeps
// `monitor now` from a bare shell consistent with the scheduled run).
func ApplyEnv(env map[string]string) {
	for _, k := range PassthroughEnv {
		if v, ok := env[k]; ok && os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}

// CaptureEnv snapshots PassthroughEnv from the current environment.
func CaptureEnv() map[string]string {
	env := map[string]string{}
	for _, k := range PassthroughEnv {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	return env
}

// TrimLog keeps monitor.log small: past max bytes, it is cut to its last keep
// bytes (at a line boundary). Safe while launchd holds it open O_APPEND.
func TrimLog(path string, max, keep int64) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || st.Size() <= max {
		return err
	}
	buf := make([]byte, keep)
	if _, err := f.ReadAt(buf, st.Size()-keep); err != nil && err != io.EOF {
		return err
	}
	for i, b := range buf {
		if b == '\n' {
			buf = buf[i+1:]
			break
		}
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err = f.WriteAt(append([]byte("… (older log lines trimmed)\n"), buf...), 0)
	return err
}
