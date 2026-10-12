package conn

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestDialSSH_cancelStalledHandshake(t *testing.T) {
	for _, withDeadline := range []bool{false, true} {
		name := "without deadline"
		if withDeadline {
			name = "before deadline"
		}
		t.Run(name, func(t *testing.T) {
			fixture := setupTunnelFixture(t)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })

			// Keep private credentials and host-key policy, but point the alias at
			// a peer that accepts TCP and never sends its SSH identification.
			stalled := *fixture
			stalled.srv = &testSSHServer{addr: ln.Addr().String()}
			stalled.writeConfig(t, "  StrictHostKeyChecking yes\n  UserKnownHostsFile "+fixture.knownPath+"\n")

			accepted := make(chan net.Conn, 1)
			go func() {
				if nc, err := ln.Accept(); err == nil {
					accepted <- nc
				}
			}()
			var ctx context.Context
			var cancel context.CancelFunc
			if withDeadline {
				ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			done := make(chan error, 1)
			go func() {
				client, err := dialSSH(ctx, fixture.alias)
				if client != nil {
					_ = client.Close()
					if err == nil {
						err = errors.New("handshake unexpectedly succeeded without a server identification")
					}
				}
				done <- err
			}()

			var peer net.Conn
			select {
			case peer = <-accepted:
			case err := <-done:
				t.Fatalf("dial ended before the handshake started: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("TCP peer was not reached")
			}
			defer peer.Close()
			if err := peer.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			banner, err := bufio.NewReader(peer).ReadString('\n')
			if err != nil || !strings.HasPrefix(banner, "SSH-") {
				t.Fatalf("client did not start its SSH handshake: banner=%q err=%v", banner, err)
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("dial error = %v, want context.Canceled", err)
				}
			case <-time.After(time.Second):
				// Unblock and join the old implementation before restoring global
				// ssh_config state. A regression must fail, not leak a goroutine.
				_ = peer.Close()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("dial did not exit even after the peer was closed")
				}
				t.Fatal("cancelled SSH handshake remained blocked waiting for the peer")
			}
		})
	}
}

func TestSSHTunnel_completedHandshakeOutlivesDialContext(t *testing.T) {
	srv := setupTunnelFixture(t).srv
	echo := startEcho(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dial := sshDialFunc()
	first, err := dial(ctx, "tcp", echo)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, first, "first request")
	client := currentTunnelClient()
	cancel()

	// An MCP request's cancellation must not close the established singleton
	// used by the next request or by another database's pool.
	nextCtx, nextCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer nextCancel()
	second, err := dial(nextCtx, "tcp", echo)
	if err != nil {
		t.Fatalf("established tunnel was tied to the first request: %v", err)
	}
	roundTrip(t, second, "next request")
	if currentTunnelClient() != client || srv.connCount() != 1 {
		t.Fatal("cancelling a completed dial replaced its healthy shared tunnel")
	}
}
