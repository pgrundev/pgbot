package conn

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSplitTunnelSpec(t *testing.T) {
	cases := []struct {
		in               string
		user, host, port string
		wantErr          bool
	}{
		{in: "lm1", host: "lm1"},
		{in: "daf@lm1", user: "daf", host: "lm1"},
		{in: "lm1:2222", host: "lm1", port: "2222"},
		{in: "daf@lm1:2222", user: "daf", host: "lm1", port: "2222"},
		{in: "bastion.example.com", host: "bastion.example.com"},
		{in: " lm1 ", host: "lm1"},
		// An IPv6 literal must keep its colons; only the bracketed form can carry
		// a port, because a bare one is ambiguous.
		{in: "[::1]:2222", host: "::1", port: "2222"},
		{in: "[fe80::1]", host: "fe80::1"},
		{in: "fe80::1", host: "fe80::1"},
		{in: "daf@[::1]:22", user: "daf", host: "::1", port: "22"},
		{in: "", wantErr: true},
		{in: "daf@", wantErr: true},
	}
	for _, c := range cases {
		user, host, port, err := splitTunnelSpec(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("splitTunnelSpec(%q): want error, got %q/%q/%q", c.in, user, host, port)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitTunnelSpec(%q): %v", c.in, err)
			continue
		}
		if user != c.user || host != c.host || port != c.port {
			t.Errorf("splitTunnelSpec(%q) = %q/%q/%q, want %q/%q/%q",
				c.in, user, host, port, c.user, c.host, c.port)
		}
	}
}

// A tunnel that was never configured must leave pgx's own dialer in place —
// otherwise every direct connection would start paying for this feature.
func TestSSHDialFunc_nilWhenUnconfigured(t *testing.T) {
	SetSSHTunnel("")
	defer SetSSHTunnel("")
	if SSHTunnelActive() {
		t.Fatal("SSHTunnelActive() true with an empty spec")
	}
	if sshDialFunc() != nil {
		t.Fatal("sshDialFunc() returned a dialer with no tunnel configured")
	}
	SetSSHTunnel("  lm1  ")
	if !SSHTunnelActive() {
		t.Fatal("SSHTunnelActive() false after SetSSHTunnel")
	}
	if sshDialFunc() == nil {
		t.Fatal("sshDialFunc() returned nil with a tunnel configured")
	}
}

func TestExpandTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	cases := map[string]string{
		"~/.ssh/id_ed25519": filepath.Join(home, ".ssh/id_ed25519"),
		"~":                 home,
		"/etc/ssh/key":      "/etc/ssh/key",
		"  ~/x  ":           filepath.Join(home, "x"),
		"":                  "",
	}
	for in, want := range cases {
		if got := expandTilde(in); got != want {
			t.Errorf("expandTilde(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsYes(t *testing.T) {
	for _, v := range []string{"yes", "YES", "Yes", "true", "on", " yes "} {
		if !isYes(v) {
			t.Errorf("isYes(%q) = false", v)
		}
	}
	for _, v := range []string{"no", "off", "", "ask", "accept-new"} {
		if isYes(v) {
			t.Errorf("isYes(%q) = true", v)
		}
	}
}

// knownHostsFiles must drop anything that verifies nothing — /dev/null (the
// idiomatic "don't check" spelling), missing files, and empty ones. Passing an
// empty file to knownhosts.New is an error, and passing /dev/null would make the
// callback reject every host instead of falling through to the configured policy.
func TestKnownHostsFiles_dropsUnusable(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(real, []byte("example.com ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "nope")

	got := filterUsableKnownHosts([]string{os.DevNull, empty, missing, real})
	if len(got) != 1 || got[0] != real {
		t.Errorf("filterUsableKnownHosts = %v, want [%s]", got, real)
	}
}

func TestAgentSocket_expandsEnvReference(t *testing.T) {
	// OpenSSH expands environment references in IdentityAgent; `$SSH_AUTH_SOCK`
	// is the common spelling and must not be taken as a literal path.
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.test")
	if got := expandAgentSpec("$SSH_AUTH_SOCK"); got != "/tmp/agent.test" {
		t.Errorf("expandAgentSpec($SSH_AUTH_SOCK) = %q", got)
	}
	if got := expandAgentSpec("SSH_AUTH_SOCK"); got != "/tmp/agent.test" {
		t.Errorf("expandAgentSpec(SSH_AUTH_SOCK) = %q", got)
	}
	if got := expandAgentSpec(""); got != "/tmp/agent.test" {
		t.Errorf("expandAgentSpec(empty) = %q", got)
	}
	if got := expandAgentSpec("none"); got != "/tmp/agent.test" {
		t.Errorf("expandAgentSpec(none) = %q", got)
	}
	if got := expandAgentSpec(`"/run/user/1000/keyring/ssh"`); got != "/run/user/1000/keyring/ssh" {
		t.Errorf("expandAgentSpec(quoted path) = %q", got)
	}
}
