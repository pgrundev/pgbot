//go:build darwin

package monitor

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Builds PgBot.app with the stock macOS tools and checks the parts Notification
// Center reads: name, bundle id, icon, and a valid signature. Posts nothing.
func TestNotifierAppBuild(t *testing.T) {
	if _, err := os.Stat("/usr/bin/osacompile"); err != nil {
		t.Skip("osacompile unavailable")
	}
	app := filepath.Join(t.TempDir(), "PgBot.app")
	n := appNotifier{app: app, bundleID: "dev.pgbot.monitor.unittest.notifier"}
	if err := n.ensure(); err != nil {
		t.Fatal(err)
	}
	read := func(key string) string {
		out, err := exec.Command("/usr/bin/plutil", "-extract", key, "raw", filepath.Join(app, "Contents", "Info.plist")).Output()
		if err != nil {
			t.Fatalf("plutil %s: %v", key, err)
		}
		return strings.TrimSpace(string(out))
	}
	for k, want := range map[string]string{
		"CFBundleName": "PgBot", "CFBundleIdentifier": "dev.pgbot.monitor.unittest.notifier",
		"CFBundleIconFile": "applet", "LSUIElement": "true",
	} {
		if got := read(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	icns, _ := os.ReadFile(filepath.Join(app, "Contents", "Resources", "applet.icns"))
	if !bytes.Equal(icns, pgbotIcns) {
		t.Error("applet.icns is not the PgBot icon")
	}
	if out, err := exec.Command("/usr/bin/codesign", "--verify", "--strict", app).CombinedOutput(); err != nil {
		t.Fatalf("signature invalid: %s", out)
	}

	// Up to date → not rebuilt.
	exe := filepath.Join(app, "Contents", "MacOS", "applet")
	before, _ := os.Stat(exe)
	if err := n.ensure(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(exe)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("an up-to-date notifier was rebuilt")
	}
	// Stale stamp (e.g. after a pgbot upgrade) → rebuilt.
	os.WriteFile(n.stampPath(app), []byte("old\n"), 0o644)
	if err := n.ensure(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(n.stampPath(app)); strings.TrimSpace(string(got)) != n.stamp() {
		t.Error("stale notifier not rebuilt")
	}
	if err := RemoveNotifierApp(app); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(app); !os.IsNotExist(err) {
		t.Error("RemoveNotifierApp left the bundle")
	}
}
