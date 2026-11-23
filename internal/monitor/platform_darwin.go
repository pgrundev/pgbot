//go:build darwin

package monitor

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Supported reports whether the monitor can run on this OS.
func Supported() bool { return true }

// NativePlatform returns the macOS adapters. Banners go through the PgBot.app
// notifier (own name + icon), falling back to plain osascript.
func NativePlatform(p Paths) Platform {
	return Platform{Secrets: keychain{}, Agents: launchd{},
		Notifier: appNotifier{app: p.NotifierApp, bundleID: p.Label + ".notifier", fallback: osaNotifier{}}}
}

// ---- Keychain ---------------------------------------------------------------
//
// The binary is built with CGO_ENABLED=0 (one static file, every platform), so
// the Security framework isn't linkable directly. /usr/bin/security is Apple's
// own Keychain client and ships with every macOS. The secret NEVER appears in
// a process argument list (visible to `ps`): writes go through `security -i`,
// which reads its command from stdin, with the value hex-encoded (-X) so no
// quoting is involved; reads use -w, which prints to our stdout pipe. The item
// lands in the user's login Keychain, readable without a prompt by the tool
// that created it — and the scheduled job reads it through that same tool.

const securityBin = "/usr/bin/security"

// secretPrefix marks the stored encoding. `security -w` prints printable
// passwords verbatim but non-ASCII ones as hex, which would be ambiguous; an
// ASCII base64 payload always round-trips exactly.
const secretPrefix = "pgbot1:"

type keychain struct{}

func (keychain) Set(service, account, secret string) error {
	if !safeKeychainName(service) || !safeKeychainName(account) {
		return fmt.Errorf("invalid keychain service/account name")
	}
	payload := secretPrefix + base64.StdEncoding.EncodeToString([]byte(secret))
	cmdline := fmt.Sprintf("add-generic-password -U -a %s -s %s -l %s -j %s -X %s\n",
		account, service, service, "PgBot-Monitor-database-connection", hex.EncodeToString([]byte(payload)))
	cmd := exec.Command(securityBin, "-i")
	cmd.Stdin = strings.NewReader(cmdline)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("keychain: could not save the connection (%s)", firstLine(out.String()))
	}
	// `security -i` exits 0 even when a command inside it fails; confirm.
	got, err := keychain{}.Get(service, account)
	if err != nil || got != secret {
		return fmt.Errorf("keychain: the connection was not saved (%s) — is the login keychain unlocked?", firstLine(out.String()))
	}
	return nil
}

func (keychain) Get(service, account string) (string, error) {
	cmd := exec.Command(securityBin, "find-generic-password", "-s", service, "-a", account, "-w")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 44 { // errSecItemNotFound
			return "", ErrNoSecret
		}
		return "", fmt.Errorf("keychain: could not read the saved connection (%s) — is the login keychain unlocked?", firstLine(stderr.String()))
	}
	v := strings.TrimRight(stdout.String(), "\r\n")
	if !strings.HasPrefix(v, secretPrefix) {
		return "", fmt.Errorf("keychain item %q was not written by pgbot", service)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(v, secretPrefix))
	if err != nil {
		return "", fmt.Errorf("keychain item %q is corrupt", service)
	}
	return string(raw), nil
}

func (keychain) Delete(service, account string) error {
	cmd := exec.Command(securityBin, "delete-generic-password", "-s", service, "-a", account)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 44 {
			return nil
		}
		return fmt.Errorf("keychain: %s", firstLine(out.String()))
	}
	return nil
}

// safeKeychainName keeps names to a charset that needs no quoting inside
// `security -i`'s command line.
func safeKeychainName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// ---- launchd ----------------------------------------------------------------

const launchctlBin = "/bin/launchctl"

type launchd struct{}

func guiDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func (launchd) Install(label, plistPath string, plist []byte) error {
	if err := os.MkdirAll(dirOf(plistPath), 0o755); err != nil {
		return err
	}
	// Unload any previous version first, so an edit replaces the job instead
	// of leaving a second one (or the old schedule) behind.
	_ = runCmd(launchctlBin, "bootout", guiDomain()+"/"+label)
	if err := writeFileAtomic(plistPath, plist, 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("/usr/bin/plutil", "-lint", plistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("launchd plist failed validation: %s", firstLine(string(out)))
	}
	// bootout completes asynchronously; bootstrap can briefly fail with EIO
	// while the old instance is still being torn down.
	var last error
	for i := 0; i < 10; i++ {
		out, err := exec.Command(launchctlBin, "bootstrap", guiDomain(), plistPath).CombinedOutput()
		if err == nil {
			return nil
		}
		last = fmt.Errorf("launchctl bootstrap: %s", firstLine(string(out)))
		time.Sleep(200 * time.Millisecond)
	}
	return last
}

func (launchd) Remove(label, plistPath string) error {
	_ = runCmd(launchctlBin, "bootout", guiDomain()+"/"+label)
	if err := os.Remove(plistPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (launchd) Loaded(label string) bool {
	return runCmd(launchctlBin, "print", guiDomain()+"/"+label) == nil
}

func (launchd) Kick(label string) error {
	if out, err := exec.Command(launchctlBin, "kickstart", guiDomain()+"/"+label).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl kickstart: %s", firstLine(string(out)))
	}
	return nil
}

// ---- Notification Center (fallback) -----------------------------------------
//
// Plain osascript `display notification`: the fallback when the PgBot.app
// notifier can't be built or launched. It works from a LaunchAgent in the
// user's GUI session, but macOS attributes it to "Script Editor" (its name and
// icon). Text is passed as argv to an `on run argv` handler — never spliced
// into the script — so no title or body can break out of the AppleScript string.

type osaNotifier struct{}

func (osaNotifier) Notify(n Notification, sound bool) error {
	script := []string{
		"on run argv",
		"display notification (item 2 of argv) with title (item 1 of argv)",
		"end run",
	}
	if sound {
		script[1] += ` sound name "default"`
	}
	args := []string{}
	for _, l := range script {
		args = append(args, "-e", l)
	}
	args = append(args, n.Title, n.Body)
	out, err := exec.Command("/usr/bin/osascript", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript: %s", firstLine(string(out)))
	}
	return nil
}

// ---- helpers ----------------------------------------------------------------

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Run()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "no details"
	}
	return Redact(s)
}

func dirOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "."
}
