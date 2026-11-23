//go:build darwin

package monitor

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The notifier app gives PgBot's banners their own name and icon. Notification
// Center attributes a banner to the app that posts it: from bare osascript that
// is "Script Editor" with its icon. So pgbot builds a tiny AppleScript applet,
// PgBot.app, with the elephant icon and its own bundle id, and posts through it.
// Everything used to build it ships with macOS (osacompile, plutil, codesign,
// lsregister) — no Xcode, no Homebrew, nothing downloaded.
//
// assets/PgBot.icns is generated from assets/pgbot-icon-1024.png with
// sips (16–256 px, @1x and @2x) + iconutil -c icns.

//go:embed assets/PgBot.icns
var pgbotIcns []byte

// notifierVersion is bumped whenever the applet's script or layout changes, so
// an existing PgBot.app is rebuilt on upgrade.
const notifierVersion = "1"

// The applet reads title, body and an optional "sound" flag from its own argv
// (`open --args`), so no text is ever spliced into AppleScript source.
var notifierScript = []string{
	`use framework "Foundation"`,
	`use scripting additions`,
	`on run`,
	`set a to (current application's NSProcessInfo's processInfo()'s arguments()) as list`,
	`if (count of a) < 3 then return`,
	`if (count of a) > 3 then`,
	`display notification (item 3 of a) with title (item 2 of a) sound name "default"`,
	`else`,
	`display notification (item 3 of a) with title (item 2 of a)`,
	`end if`,
	`delay 0.5`,
	`end run`,
}

const lsregister = "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"

type appNotifier struct {
	app      string // …/PgBot.app
	bundleID string
	fallback Notifier
}

func (n appNotifier) stamp() string {
	h := sha256.New()
	h.Write(pgbotIcns)
	h.Write([]byte(strings.Join(notifierScript, "\n")))
	h.Write([]byte(n.bundleID + "|" + notifierVersion))
	return hex.EncodeToString(h.Sum(nil))
}

func (n appNotifier) stampPath(app string) string {
	return filepath.Join(app, "Contents", "Resources", "pgbot-notifier.stamp")
}

// ensure builds PgBot.app if it is missing or out of date.
func (n appNotifier) ensure() error {
	want := n.stamp()
	if got, err := os.ReadFile(n.stampPath(n.app)); err == nil && strings.TrimSpace(string(got)) == want {
		return nil
	}
	parent := filepath.Dir(n.app)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp(parent, ".pgbot-notifier-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	tmp := filepath.Join(tmpDir, "PgBot.app")

	args := []string{"-o", tmp}
	for _, l := range notifierScript {
		args = append(args, "-e", l)
	}
	if out, err := exec.Command("/usr/bin/osacompile", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("osacompile: %s", firstLine(string(out)))
	}
	if err := os.WriteFile(filepath.Join(tmp, "Contents", "Resources", "applet.icns"), pgbotIcns, 0o644); err != nil {
		return err
	}
	info := filepath.Join(tmp, "Contents", "Info.plist")
	for _, kv := range [][]string{
		{"CFBundleName", "-string", "PgBot"},
		{"CFBundleDisplayName", "-string", "PgBot"},
		{"CFBundleIdentifier", "-string", n.bundleID},
		{"CFBundleIconFile", "-string", "applet"},
		{"LSUIElement", "-bool", "YES"}, // no Dock icon while it posts
	} {
		if out, err := exec.Command("/usr/bin/plutil", "-replace", kv[0], kv[1], kv[2], info).CombinedOutput(); err != nil {
			return fmt.Errorf("plutil: %s", firstLine(string(out)))
		}
	}
	// A stale asset catalog would override the icon; osacompile doesn't emit
	// one today, but don't let a future version win silently.
	_ = os.Remove(filepath.Join(tmp, "Contents", "Resources", "Assets.car"))
	_ = exec.Command("/usr/bin/plutil", "-remove", "CFBundleIconName", info).Run()
	if err := os.WriteFile(n.stampPath(tmp), []byte(n.stamp()+"\n"), 0o644); err != nil {
		return err
	}
	// Editing Info.plist and Resources broke osacompile's ad-hoc seal; re-sign
	// ad hoc so the bundle validates (required to launch on Apple silicon).
	if out, err := exec.Command("/usr/bin/codesign", "--force", "--sign", "-", tmp).CombinedOutput(); err != nil {
		return fmt.Errorf("codesign: %s", firstLine(string(out)))
	}
	if err := os.RemoveAll(n.app); err != nil {
		return err
	}
	if err := os.Rename(tmp, n.app); err != nil {
		return err
	}
	// Register so Notification Center knows the bundle (name + icon) before
	// its first banner. Best-effort: `open` registers it too.
	_ = exec.Command(lsregister, "-f", n.app).Run()
	return nil
}

func (n appNotifier) Notify(note Notification, sound bool) error {
	if err := n.ensure(); err != nil {
		return n.fallback.Notify(note, sound)
	}
	args := []string{"-g", "-n", "-W", "-a", n.app, "--args", note.Title, note.Body}
	if sound {
		args = append(args, "sound")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/open", args...).CombinedOutput()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("the PgBot notifier did not finish in time")
		}
		// The helper couldn't launch (removed, damaged, blocked): still deliver.
		if ferr := n.fallback.Notify(note, sound); ferr == nil {
			return nil
		}
		return fmt.Errorf("open PgBot.app: %s", firstLine(string(out)))
	}
	return nil
}

// RemoveNotifierApp unregisters and deletes PgBot.app (used by stop --purge).
func RemoveNotifierApp(app string) error {
	if _, err := os.Stat(app); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	_ = exec.Command(lsregister, "-u", app).Run()
	return os.RemoveAll(app)
}
