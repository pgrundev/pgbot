package conn

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kevinburke/ssh_config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// The unit tests cover spec parsing and the config helpers; nothing exercised
// dialSSH, the known_hosts policy, or the reconnect path. This spins up a real
// SSH server in-process (host key, publickey auth, direct-tcpip forwarding) and
// drives sshDialFunc through it against an echo listener, so the whole chain —
// ssh_config lookup → IdentityFile → StrictHostKeyChecking=yes against a
// known_hosts entry → channel open → bytes through — is pinned end to end.

// testSSHServer is a minimal jump host: it accepts one authorized key and
// forwards direct-tcpip channels to wherever they ask.
type testSSHServer struct {
	addr  string
	mu    sync.Mutex
	conns []*ssh.ServerConn
}

func startTestSSHServer(t *testing.T, hostKey ssh.Signer, authorized ssh.PublicKey) *testSSHServer {
	t.Helper()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(k.Marshal(), authorized.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.AddHostKey(hostKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &testSSHServer{addr: ln.Addr().String()}
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(nc, cfg)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		s.dropAll()
	})
	return s
}

func (s *testSSHServer) serve(nc net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		_ = nc.Close()
		return
	}
	s.mu.Lock()
	s.conns = append(s.conns, sc)
	s.mu.Unlock()
	go ssh.DiscardRequests(reqs)
	for ch := range chans {
		if ch.ChannelType() != "direct-tcpip" {
			_ = ch.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		var p struct {
			Host  string
			Port  uint32
			OHost string
			OPort uint32
		}
		if err := ssh.Unmarshal(ch.ExtraData(), &p); err != nil {
			_ = ch.Reject(ssh.ConnectionFailed, err.Error())
			continue
		}
		dst, err := net.Dial("tcp", net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port))))
		if err != nil {
			_ = ch.Reject(ssh.ConnectionFailed, err.Error())
			continue
		}
		c, creqs, err := ch.Accept()
		if err != nil {
			_ = dst.Close()
			continue
		}
		go ssh.DiscardRequests(creqs)
		go func() { _, _ = io.Copy(c, dst); _ = c.Close() }()
		go func() { _, _ = io.Copy(dst, c); _ = dst.Close() }()
	}
}

// dropAll closes every server-side SSH connection — the jump host "went away".
func (s *testSSHServer) dropAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.conns = nil
}

func (s *testSSHServer) connCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// startEcho is the "database": a TCP listener that echoes what it reads.
func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

func newSigner(t *testing.T) (ssh.Signer, ed25519.PrivateKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer, priv
}

// tunnelFixture is a jump host alias wired through a private ssh_config so the
// developer's own ~/.ssh/config, agent, and known_hosts never take part.
type tunnelFixture struct {
	srv        *testSSHServer
	alias      string
	dir        string
	keyPath    string // client IdentityFile
	knownPath  string // UserKnownHostsFile
	cfgPath    string
	hostSigner ssh.Signer
}

// writeConfig (re)writes the alias block. policy is appended verbatim — the
// StrictHostKeyChecking / UserKnownHostsFile lines a test wants in effect.
func (f *tunnelFixture) writeConfig(t *testing.T, policy string) {
	t.Helper()
	host, port, err := net.SplitHostPort(f.srv.addr)
	if err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("Host %s\n  HostName %s\n  Port %s\n  User tester\n  IdentityFile %s\n  IdentitiesOnly yes\n  IdentityAgent none\n%s",
		f.alias, host, port, f.keyPath, policy)
	if err := os.WriteFile(f.cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	// ssh_config caches the parsed file per UserSettings; a fresh one re-reads.
	us := &ssh_config.UserSettings{}
	us.ConfigFinder(func() string { return f.cfgPath })
	ssh_config.DefaultUserSettings = us
}

func setupTunnelFixture(t *testing.T) *tunnelFixture {
	t.Helper()
	dir := t.TempDir()
	hostSigner, _ := newSigner(t)
	clientSigner, clientPriv := newSigner(t)
	f := &tunnelFixture{
		srv:        startTestSSHServer(t, hostSigner, clientSigner.PublicKey()),
		alias:      "pgbot-test-jump",
		dir:        dir,
		keyPath:    filepath.Join(dir, "id_ed25519"),
		knownPath:  filepath.Join(dir, "known_hosts"),
		cfgPath:    filepath.Join(dir, "ssh_config"),
		hostSigner: hostSigner,
	}
	block, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	line := knownhosts.Line([]string{f.srv.addr}, hostSigner.PublicKey())
	if err := os.WriteFile(f.knownPath, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	prev := ssh_config.DefaultUserSettings
	f.writeConfig(t, "  StrictHostKeyChecking yes\n  UserKnownHostsFile "+f.knownPath+"\n")
	t.Setenv("SSH_AUTH_SOCK", "")
	SetSSHTunnel(f.alias)
	t.Cleanup(func() {
		CloseSSHTunnel()
		SetSSHTunnel("")
		ssh_config.DefaultUserSettings = prev
	})
	return f
}

func roundTrip(t *testing.T, nc net.Conn, msg string) {
	t.Helper()
	defer nc.Close()
	_ = nc.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := nc.Write([]byte(msg)); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(nc, buf); err != nil {
		t.Fatalf("read through tunnel: %v", err)
	}
	if string(buf) != msg {
		t.Fatalf("echo through tunnel = %q; want %q", buf, msg)
	}
}

func currentTunnelClient() *ssh.Client {
	tunnelMu.Lock()
	defer tunnelMu.Unlock()
	return tunnelConn
}

func TestSSHTunnel_endToEnd(t *testing.T) {
	srv := setupTunnelFixture(t).srv
	echo := startEcho(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dial := sshDialFunc()
	if dial == nil {
		t.Fatal("sshDialFunc() = nil with a tunnel configured")
	}
	nc, err := dial(ctx, "tcp", echo)
	if err != nil {
		t.Fatalf("dial through tunnel: %v", err)
	}
	roundTrip(t, nc, "hello via jump host")

	// A second database connection rides the same SSH connection.
	nc2, err := dial(ctx, "tcp", echo)
	if err != nil {
		t.Fatalf("second dial through tunnel: %v", err)
	}
	roundTrip(t, nc2, "second channel")
	if n := srv.connCount(); n != 1 {
		t.Fatalf("server saw %d SSH connections for two dials; want 1 (shared client)", n)
	}
}

// The jump host drops the transport (idle timeout, sleeping laptop): the next
// dial must re-establish the SSH connection once rather than fail for the rest
// of the process.
func TestSSHTunnel_redialsAfterTransportLoss(t *testing.T) {
	srv := setupTunnelFixture(t).srv
	echo := startEcho(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dial := sshDialFunc()

	nc, err := dial(ctx, "tcp", echo)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, nc, "before drop")
	first := currentTunnelClient()

	srv.dropAll()
	// Let the client's transport observe the close so the failure is the
	// "dead client" case rather than a write racing the FIN.
	_ = first.Wait()

	nc, err = dial(ctx, "tcp", echo)
	if err != nil {
		t.Fatalf("dial after transport loss: %v", err)
	}
	roundTrip(t, nc, "after redial")
	if currentTunnelClient() == first {
		t.Fatal("tunnel client was not replaced after the transport died")
	}
	if n := srv.connCount(); n != 1 {
		t.Fatalf("server holds %d live SSH connections after redial; want 1", n)
	}
}

// Concurrent pool dials can race the redial: goroutine A sees its dial fail on
// the dead client, goroutine B has already replaced it. A must not close B's
// healthy replacement on its way to re-dialing.
func TestDropTunnelClient_onlyDropsTheCurrentClient(t *testing.T) {
	alias := setupTunnelFixture(t).alias
	echo := startEcho(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stale, err := dialSSH(ctx, alias)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := dialSSH(ctx, alias)
	if err != nil {
		t.Fatal(err)
	}
	tunnelMu.Lock()
	tunnelConn = fresh
	tunnelMu.Unlock()

	dropTunnelClient(stale) // A's late drop of a client B already replaced
	if got := currentTunnelClient(); got != fresh {
		t.Fatalf("dropping a stale client replaced the shared one: got %p, want %p", got, fresh)
	}
	nc, err := fresh.DialContext(ctx, "tcp", echo)
	if err != nil {
		t.Fatalf("the replacement client was closed by a stale drop: %v", err)
	}
	roundTrip(t, nc, "still open")

	dropTunnelClient(fresh) // the current client really is dropped
	if currentTunnelClient() != nil {
		t.Fatal("dropTunnelClient left the current client in place")
	}
	_ = stale.Close()
}

// The jump host is up but refuses the forward (the database host is unreachable
// from there, or forwarding is prohibited). That is not a dead transport: the
// shared client must survive it, or one bad target would sever every other
// pool connection riding the tunnel.
func TestSSHTunnel_refusedForwardKeepsTheClient(t *testing.T) {
	srv := setupTunnelFixture(t).srv
	echo := startEcho(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dial := sshDialFunc()

	nc, err := dial(ctx, "tcp", echo)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, nc, "warm")
	client := currentTunnelClient()

	// A listener that is closed before anyone dials it: the server's own
	// net.Dial fails and it rejects the channel.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := dead.Addr().String()
	_ = dead.Close()

	if _, err := dial(ctx, "tcp", deadAddr); err == nil {
		t.Fatal("dial to a closed target through the tunnel succeeded")
	}
	if currentTunnelClient() != client {
		t.Fatal("a refused forward replaced the shared SSH client")
	}
	if n := srv.connCount(); n != 1 {
		t.Fatalf("server saw %d SSH connections after a refused forward; want 1", n)
	}
	nc, err = dial(ctx, "tcp", echo)
	if err != nil {
		t.Fatalf("dial after a refused forward: %v", err)
	}
	roundTrip(t, nc, "still riding the same client")
}

func TestTransportDead(t *testing.T) {
	ctx := context.Background()
	if transportDead(ctx, &ssh.OpenChannelError{Reason: ssh.ConnectionFailed, Message: "connect failed"}) {
		t.Error("a channel-open rejection was classed as a dead transport")
	}
	expired, cancel := context.WithCancel(ctx)
	cancel()
	if transportDead(expired, errors.New("ssh: unexpected packet in response to channel open: <nil>")) {
		t.Error("a failure under an expired context was classed as a dead transport")
	}
	if !transportDead(ctx, io.EOF) {
		t.Error("EOF on a live context was not classed as a dead transport")
	}
}

// StrictHostKeyChecking=yes must refuse a jump host whose key is not the one in
// known_hosts — the interception case, and the whole reason the policy is read
// from the user's own config rather than invented here.
func TestSSHTunnel_refusesChangedHostKey(t *testing.T) {
	f := setupTunnelFixture(t)
	other, _ := newSigner(t)
	stale := knownhosts.Line([]string{f.srv.addr}, other.PublicKey())
	if err := os.WriteFile(f.knownPath, []byte(stale+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, policy := range []string{"yes", "accept-new", "ask", ""} {
		f.writeConfig(t, "  StrictHostKeyChecking "+policy+"\n  UserKnownHostsFile "+f.knownPath+"\n")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := dialSSH(ctx, f.alias)
		cancel()
		if err == nil {
			t.Fatalf("StrictHostKeyChecking=%q: dialSSH accepted a jump host whose key does not match known_hosts", policy)
		}
		if want := "does not match known_hosts"; !bytes.Contains([]byte(err.Error()), []byte(want)) {
			t.Fatalf("StrictHostKeyChecking=%q: error = %q; want it to mention %q", policy, err, want)
		}
	}
}

// Under accept-new (and the non-interactive reading of ask) a first-sight host
// is accepted — and must then be RECORDED, so the next run can tell a changed
// key from a new host. Without the record, a changed key is forever "unknown".
func TestSSHTunnel_recordsAcceptedHostKey(t *testing.T) {
	f := setupTunnelFixture(t)
	fresh := filepath.Join(f.dir, "nested", "known_hosts") // absent: also proves the dir is created
	f.writeConfig(t, "  StrictHostKeyChecking accept-new\n  UserKnownHostsFile "+fresh+"\n")
	echo := startEcho(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	nc, err := sshDialFunc()(ctx, "tcp", echo)
	if err != nil {
		t.Fatalf("first-sight dial under accept-new: %v", err)
	}
	roundTrip(t, nc, "first sight")

	raw, err := os.ReadFile(fresh)
	if err != nil {
		t.Fatalf("accepted host key was not recorded: %v", err)
	}
	want := knownhosts.Line([]string{f.srv.addr}, f.hostSigner.PublicKey())
	if string(bytes.TrimSpace(raw)) != want {
		t.Fatalf("recorded known_hosts line = %q; want %q", bytes.TrimSpace(raw), want)
	}

	// The recorded key now pins the host: a different key at the same address is
	// a change, not a first sight, under every policy except an explicit no.
	cb, err := hostKeyCallback(f.alias)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := net.ResolveTCPAddr("tcp", f.srv.addr)
	if err != nil {
		t.Fatal(err)
	}
	impostor, _ := newSigner(t)
	if err := cb(f.srv.addr, remote, impostor.PublicKey()); err == nil {
		t.Fatal("a changed host key was accepted after the original had been recorded")
	}
	if err := cb(f.srv.addr, remote, f.hostSigner.PublicKey()); err != nil {
		t.Fatalf("the recorded key itself was refused: %v", err)
	}
}

// IdentityAgent none must keep the agent out of the auth chain. With no
// IdentityFile either, that leaves no credentials at all — the honest outcome,
// rather than silently reaching for SSH_AUTH_SOCK the user told us not to use.
func TestSSHTunnel_identityAgentNoneDisablesAgent(t *testing.T) {
	f := setupTunnelFixture(t)
	t.Setenv("SSH_AUTH_SOCK", "/nonexistent/agent.sock")
	host, port, _ := net.SplitHostPort(f.srv.addr)
	cfg := fmt.Sprintf("Host %s\n  HostName %s\n  Port %s\n  User tester\n  IdentityAgent none\n  StrictHostKeyChecking yes\n  UserKnownHostsFile %s\n",
		f.alias, host, port, f.knownPath)
	if err := os.WriteFile(f.cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	us := &ssh_config.UserSettings{}
	us.ConfigFinder(func() string { return f.cfgPath })
	ssh_config.DefaultUserSettings = us

	h, err := resolveSSHHost(f.alias)
	if err != nil {
		t.Fatal(err)
	}
	if sock := agentSocket(h.alias); sock != "" {
		t.Fatalf("agentSocket with IdentityAgent none = %q; want \"\"", sock)
	}
	auths, closeAgent, err := sshAuthMethods(h)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAgent()
	if len(auths) != 0 {
		t.Fatalf("sshAuthMethods offered %d method(s) with IdentityAgent none and no IdentityFile; want 0", len(auths))
	}
}
