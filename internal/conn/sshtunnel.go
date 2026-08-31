package conn

// SSH tunnelling. pgbot's connection path is libpq-only: it reaches whatever the
// DSN's host resolves to, which leaves out every database that only answers from
// inside a bastion, a VPN-routed jump host, or a private VPC subnet.
//
// The tunnel is installed as pgx's DialFunc rather than as a local port forward.
// That distinction matters: pgconn documents DialFunc as running BEFORE TLS is
// established, so the DSN keeps naming the REAL host all the way through.
// sslmode=verify-full still validates against that hostname, and .pgpass still
// matches on it. A `ssh -L` forward would force the DSN to say 127.0.0.1 and
// silently break both, besides leaving a port open to every local user.
//
// Host identity is NOT pgbot's policy to invent — it reads StrictHostKeyChecking
// and UserKnownHostsFile out of ssh_config and behaves the way the user's own ssh
// already does for that host.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kevinburke/ssh_config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/term"
)

// SSHTunnelEnv is the environment variable that configures the jump host when
// --ssh-tunnel isn't passed.
const SSHTunnelEnv = "PGBOT_SSH_TUNNEL"

// tunnel state. The client is a process-wide singleton: --all-databases opens one
// Target per database and `pgbot mcp` opens one per request, and every one of them
// should ride the same SSH connection rather than re-authenticating.
var (
	tunnelMu   sync.Mutex
	tunnelSpec string
	tunnelConn *ssh.Client
)

// SetSSHTunnel configures the jump host every subsequent Connect dials through.
// Spec is `[user@]host[:port]`, where host is looked up in ssh_config exactly as
// the ssh client would resolve it — so a bare alias picks up its HostName, User,
// Port and IdentityFile. An explicit user or port in the spec wins over the file.
// Empty spec (the default) means connect directly, and costs nothing.
func SetSSHTunnel(spec string) {
	tunnelMu.Lock()
	defer tunnelMu.Unlock()
	tunnelSpec = strings.TrimSpace(spec)
}

// SSHTunnelActive reports whether a jump host is configured, for callers that
// want to say so in their header line.
func SSHTunnelActive() bool {
	tunnelMu.Lock()
	defer tunnelMu.Unlock()
	return tunnelSpec != ""
}

// CloseSSHTunnel tears down the shared SSH connection. Safe to call when none was
// ever opened.
func CloseSSHTunnel() {
	tunnelMu.Lock()
	defer tunnelMu.Unlock()
	if tunnelConn != nil {
		_ = tunnelConn.Close()
		tunnelConn = nil
	}
}

// sshDialFunc returns a dialer that opens the database connection as a channel on
// the SSH connection, or nil when no tunnel is configured (pgx then keeps its own
// default dialer, timeouts included).
func sshDialFunc() func(context.Context, string, string) (net.Conn, error) {
	if !SSHTunnelActive() {
		return nil
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := tunnelClient(ctx)
		if err != nil {
			return nil, err
		}
		nc, err := c.DialContext(ctx, network, addr)
		if err == nil {
			return nc, nil
		}
		// A pooled connection can outlive the SSH transport (an idle timeout on the
		// jump host, a laptop that slept, a VPN that flapped). Drop the dead client
		// and re-dial once before surfacing the failure — the pool would otherwise
		// stay broken for the rest of a long-lived `pgbot mcp` process.
		CloseSSHTunnel()
		c, rerr := tunnelClient(ctx)
		if rerr != nil {
			return nil, fmt.Errorf("%w (reconnect failed: %v)", err, rerr)
		}
		return c.DialContext(ctx, network, addr)
	}
}

// tunnelClient returns the shared SSH connection, dialing it on first use.
func tunnelClient(ctx context.Context) (*ssh.Client, error) {
	tunnelMu.Lock()
	defer tunnelMu.Unlock()
	if tunnelConn != nil {
		return tunnelConn, nil
	}
	if tunnelSpec == "" {
		return nil, errors.New("no ssh tunnel configured")
	}
	c, err := dialSSH(ctx, tunnelSpec)
	if err != nil {
		return nil, fmt.Errorf("ssh tunnel %q: %w", tunnelSpec, err)
	}
	tunnelConn = c
	return c, nil
}

// sshHost is a jump host resolved from the spec plus ssh_config.
type sshHost struct {
	alias   string   // what the user typed — the ssh_config lookup key
	addr    string   // host:port actually dialed
	user    string   // login user
	keys    []string // IdentityFile paths, in config order
	idsOnly bool     // IdentitiesOnly=yes — offer only the IdentityFile identities
}

// dialSSH resolves the spec against ssh_config and opens the SSH connection.
func dialSSH(ctx context.Context, spec string) (*ssh.Client, error) {
	h, err := resolveSSHHost(spec)
	if err != nil {
		return nil, err
	}
	auths, err := sshAuthMethods(h)
	if err != nil {
		return nil, err
	}
	if len(auths) == 0 {
		return nil, fmt.Errorf("no usable credentials: no key in the agent and no readable IdentityFile for %q", h.alias)
	}
	hkcb, err := hostKeyCallback(h.alias)
	if err != nil {
		return nil, err
	}

	// Dial the TCP leg through a context-aware dialer so a hung jump host respects
	// the run's deadline instead of blocking until the TCP stack gives up.
	var d net.Dialer
	rawConn, err := d.DialContext(ctx, "tcp", h.addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", h.addr, err)
	}
	cfg := &ssh.ClientConfig{
		User:            h.user,
		Auth:            auths,
		HostKeyCallback: hkcb,
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = rawConn.SetDeadline(dl)
	}
	sc, chans, reqs, err := ssh.NewClientConn(rawConn, h.addr, cfg)
	if err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("handshake with %s: %w", h.addr, err)
	}
	// Clear the handshake deadline: it was for the handshake, and leaving it set
	// would expire every database query that rides this connection later.
	_ = rawConn.SetDeadline(time.Time{})
	return ssh.NewClient(sc, chans, reqs), nil
}

// splitTunnelSpec breaks `[user@]host[:port]` apart. Bracketed IPv6 literals keep
// their colons; a bare IPv6 address has no unambiguous port syntax, so its colons
// are left alone too and the port comes from ssh_config.
func splitTunnelSpec(spec string) (user, host, port string, err error) {
	rest := strings.TrimSpace(spec)
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		user, rest = rest[:i], rest[i+1:]
	}
	switch {
	case strings.HasPrefix(rest, "["):
		// [::1]:2222 or [::1]
		if end := strings.LastIndex(rest, "]"); end > 0 {
			if tail := rest[end+1:]; strings.HasPrefix(tail, ":") {
				port = tail[1:]
			}
			rest = rest[1:end]
		}
	case strings.Count(rest, ":") == 1:
		i := strings.LastIndex(rest, ":")
		rest, port = rest[:i], rest[i+1:]
	}
	if rest == "" {
		return "", "", "", fmt.Errorf("malformed tunnel spec %q — want [user@]host[:port]", spec)
	}
	return user, rest, port, nil
}

// resolveSSHHost merges the spec with ssh_config. The spec's user/port win,
// because an explicit flag should not be silently overridden by a config file.
func resolveSSHHost(spec string) (sshHost, error) {
	user, alias, port, err := splitTunnelSpec(spec)
	if err != nil {
		return sshHost{}, err
	}
	h := sshHost{alias: alias, user: user}

	host := ssh_config.Get(h.alias, "HostName")
	if host == "" {
		host = h.alias
	}
	if port == "" {
		if port = ssh_config.Get(h.alias, "Port"); port == "" {
			port = "22"
		}
	}
	h.addr = net.JoinHostPort(host, port)

	if h.user == "" {
		if h.user = ssh_config.Get(h.alias, "User"); h.user == "" {
			// ssh falls back to the local login name.
			if u := os.Getenv("USER"); u != "" {
				h.user = u
			} else {
				h.user = os.Getenv("LOGNAME")
			}
		}
	}
	for _, k := range ssh_config.GetAll(h.alias, "IdentityFile") {
		if p := expandTilde(k); p != "" {
			h.keys = append(h.keys, p)
		}
	}
	// IdentitiesOnly=yes does NOT disable the agent — OpenSSH still uses agent-held
	// keys, it just restricts the offer to the identities named here. Treating it
	// as "no agent" breaks the common setup of an encrypted key that lives only in
	// the agent.
	h.idsOnly = isYes(ssh_config.Get(h.alias, "IdentitiesOnly"))
	return h, nil
}

// sshAuthMethods builds the auth chain: the agent first (it holds keys pgbot
// cannot read off disk, and never exposes the private material), then each
// readable IdentityFile.
func sshAuthMethods(h sshHost) ([]ssh.AuthMethod, error) {
	var out []ssh.AuthMethod
	if sock := agentSocket(h.alias); sock != "" {
		if ac, err := net.Dial("unix", sock); err == nil {
			client := agent.NewClient(ac)
			signers := client.Signers
			if h.idsOnly {
				signers = onlyIdentities(client.Signers, h.keys)
			}
			out = append(out, ssh.PublicKeysCallback(signers))
		}
	}
	for _, path := range h.keys {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue // a listed-but-absent IdentityFile is normal; ssh skips it too
		}
		signer, err := ssh.ParsePrivateKey(raw)
		if err != nil {
			var pm *ssh.PassphraseMissingError
			if !errors.As(err, &pm) {
				warnOnce(path, fmt.Sprintf("pgbot: ignoring unusable key %s: %v", path, err))
				continue
			}
			signer, err = promptForKey(path, raw)
			if err != nil {
				warnOnce(path, fmt.Sprintf("pgbot: skipping %s: %v", path, err))
				continue
			}
		}
		out = append(out, ssh.PublicKeys(signer))
	}
	return out, nil
}

// onlyIdentities implements IdentitiesOnly against the agent: keep just the
// agent-held keys whose public half matches one of the configured IdentityFiles.
// When no .pub is readable there is nothing to match on, so the full set is
// offered rather than authenticating with nothing.
func onlyIdentities(next func() ([]ssh.Signer, error), keys []string) func() ([]ssh.Signer, error) {
	return func() ([]ssh.Signer, error) {
		all, err := next()
		if err != nil {
			return nil, err
		}
		want := map[string]bool{}
		for _, k := range keys {
			pub, err := os.ReadFile(k + ".pub")
			if err != nil {
				continue
			}
			if pk, _, _, _, err := ssh.ParseAuthorizedKey(pub); err == nil {
				want[string(pk.Marshal())] = true
			}
		}
		if len(want) == 0 {
			return all, nil
		}
		var keep []ssh.Signer
		for _, s := range all {
			if want[string(s.PublicKey().Marshal())] {
				keep = append(keep, s)
			}
		}
		if len(keep) == 0 {
			return all, nil
		}
		return keep, nil
	}
}

// promptForKey unlocks a passphrase-protected key. Only on a TTY: in CI the right
// answer is to load the key into an agent, not to hang waiting on stdin.
func promptForKey(path string, raw []byte) (ssh.Signer, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, errors.New("key is passphrase-protected and there is no terminal to ask on — add it to your ssh-agent")
	}
	fmt.Fprintf(os.Stderr, "Enter passphrase for %s: ", path)
	pw, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKeyWithPassphrase(raw, pw)
}

// agentSocket honours IdentityAgent before falling back to SSH_AUTH_SOCK.
// OpenSSH expands environment references in this value — `IdentityAgent
// $SSH_AUTH_SOCK` is the idiomatic way to spell "whatever agent this shell has"
// — and accepts the bare name SSH_AUTH_SOCK for the same thing. Taking the value
// literally yields a path that cannot be dialed, and the agent silently drops out
// of the auth chain.
func agentSocket(alias string) string {
	return expandAgentSpec(ssh_config.Get(alias, "IdentityAgent"))
}

// expandAgentSpec turns an IdentityAgent value into a socket path, falling back
// to SSH_AUTH_SOCK for the empty, "none", and unresolvable cases.
func expandAgentSpec(ia string) string {
	ia = strings.Trim(strings.TrimSpace(ia), `"`)
	switch {
	case ia == "", strings.EqualFold(ia, "none"), ia == "SSH_AUTH_SOCK":
		return os.Getenv("SSH_AUTH_SOCK")
	}
	if p := expandTilde(os.ExpandEnv(ia)); p != "" {
		return p
	}
	return os.Getenv("SSH_AUTH_SOCK")
}

// hostKeyCallback reproduces the user's own ssh policy for this host rather than
// imposing one. StrictHostKeyChecking=no/off accepts anything; accept-new (and
// ask, which pgbot cannot honour non-interactively) accepts a host it has never
// seen but still refuses one whose key CHANGED — the case that actually signals
// interception. A changed key is refused under every mode except an explicit no.
func hostKeyCallback(alias string) (ssh.HostKeyCallback, error) {
	strict := strings.ToLower(ssh_config.Get(alias, "StrictHostKeyChecking"))
	files := knownHostsFiles(alias)

	if len(files) == 0 {
		if strict == "yes" {
			return nil, errors.New("StrictHostKeyChecking=yes but no readable UserKnownHostsFile — cannot verify the jump host")
		}
		fmt.Fprintf(os.Stderr, "pgbot: ssh host key for %q is not being verified (no known_hosts in effect)\n", alias)
		return ssh.InsecureIgnoreHostKey(), nil
	}

	base, err := knownhosts.New(files...)
	if err != nil {
		return nil, fmt.Errorf("read known_hosts: %w", err)
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := base(hostname, remote, key)
		if err == nil {
			return nil
		}
		var ke *knownhosts.KeyError
		if !errors.As(err, &ke) {
			return err
		}
		if len(ke.Want) > 0 {
			// The host is known and presented a DIFFERENT key. Never auto-accept.
			if strict == "no" || strict == "off" {
				fmt.Fprintf(os.Stderr, "pgbot: WARNING — host key for %q CHANGED; continuing because StrictHostKeyChecking=no\n", alias)
				return nil
			}
			return fmt.Errorf("host key for %q does not match known_hosts — refusing to connect", alias)
		}
		// Unknown host.
		switch strict {
		case "yes":
			return fmt.Errorf("host %q is not in known_hosts and StrictHostKeyChecking=yes", alias)
		default:
			fmt.Fprintf(os.Stderr, "pgbot: accepting unknown ssh host key for %q (%s)\n", alias, ssh.FingerprintSHA256(key))
			return nil
		}
	}, nil
}

// knownHostsFiles resolves UserKnownHostsFile to the files that actually exist.
// /dev/null is a deliberate "don't verify" and is dropped along with the rest.
func knownHostsFiles(alias string) []string {
	var specified []string
	for _, v := range ssh_config.GetAll(alias, "UserKnownHostsFile") {
		specified = append(specified, strings.Fields(v)...)
	}
	if len(specified) == 0 {
		specified = []string{"~/.ssh/known_hosts", "~/.ssh/known_hosts2"}
	}
	return filterUsableKnownHosts(specified)
}

// filterUsableKnownHosts keeps only paths that can actually verify a host key.
// /dev/null is the idiomatic "don't verify" spelling, and an absent or empty file
// verifies nothing — knownhosts.New errors on those, and keeping them would turn
// "unverified" into "every host rejected".
func filterUsableKnownHosts(paths []string) []string {
	var out []string
	for _, f := range paths {
		p := expandTilde(strings.Trim(f, `"`))
		if p == "" || p == os.DevNull {
			continue
		}
		if st, err := os.Stat(p); err != nil || st.IsDir() || st.Size() == 0 {
			continue
		}
		out = append(out, p)
	}
	return out
}

// expandTilde resolves a leading ~ or ~/ against the home directory.
func expandTilde(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	return p
}

// warnOnce prints a per-key diagnostic a single time. pgx dials more than once
// (probe, then each pool connection, then any fallback host), and repeating the
// same "skipping key" line four times reads like four different problems.
var warned sync.Map

func warnOnce(key, msg string) {
	if _, dup := warned.LoadOrStore(key, true); !dup {
		fmt.Fprintln(os.Stderr, msg)
	}
}

// isYes reports whether an ssh_config boolean is on.
func isYes(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "yes", "true", "on":
		return true
	}
	return false
}
