package provision

import (
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// flakyEntrance stands in front of an SSH server and closes the first drop
// connections, writing sendFirst before each close, then forwards the rest.
// sshd past MaxStartups writes "Exceeded MaxStartups\r\n" and closes.
func flakyEntrance(t *testing.T, backend string, drop int32, sendFirst string) (string, int, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var accepted atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if accepted.Add(1) <= drop {
				if sendFirst != "" {
					_, _ = io.WriteString(conn, sendFirst)
				}
				_ = conn.Close()
				continue
			}
			go func() {
				defer conn.Close()
				up, err := net.Dial("tcp", backend)
				if err != nil {
					return
				}
				defer up.Close()
				go func() { _, _ = io.Copy(up, conn) }()
				_, _ = io.Copy(conn, up)
			}()
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port, &accepted
}

func fastConnectBackoff(t *testing.T) {
	t.Helper()
	old := sshConnectBackoff
	sshConnectBackoff = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { sshConnectBackoff = old })
}

func fakeBackend(t *testing.T) string {
	host, port := startFakeSSHServer(t, "root", "pw", func(string, io.Reader) (string, string, uint32) { return "", "", 0 })
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// Connections sshd drops past MaxStartups are tried again, and the login
// then succeeds.
func TestDialSSHRetriesConnectionsDroppedPastMaxStartups(t *testing.T) {
	fastConnectBackoff(t)
	host, port, accepted := flakyEntrance(t, fakeBackend(t), 3, "Exceeded MaxStartups\r\n")
	client, outcome, err := DialSSH(context.Background(), "root", host, port, SSHAuth{Password: "pw"}, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("DialSSH: %v", err)
	}
	defer client.Close()
	if accepted.Load() != 4 || outcome.Used != AuthPassword {
		t.Fatalf("connections = %d, outcome = %+v", accepted.Load(), outcome)
	}
}

// An endpoint that closes every connection fails after the bounded
// attempts, saying so.
func TestDialSSHGivesUpOnAnEndpointThatAlwaysCloses(t *testing.T) {
	fastConnectBackoff(t)
	host, port, accepted := flakyEntrance(t, fakeBackend(t), 1000, "Exceeded MaxStartups\r\n")
	_, _, err := DialSSH(context.Background(), "root", host, port, SSHAuth{Password: "pw"}, 5*time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), `closed all 8 connections before identifying itself: "Exceeded MaxStartups"`) {
		t.Fatalf("err = %v", err)
	}
	if accepted.Load() != sshConnectAttempts {
		t.Fatalf("connections = %d, want %d", accepted.Load(), sshConnectAttempts)
	}
}

// An endpoint closing without a word is tried again too.
func TestDialSSHRetriesConnectionsClosedSilently(t *testing.T) {
	fastConnectBackoff(t)
	host, port, accepted := flakyEntrance(t, fakeBackend(t), 2, "")
	client, _, err := DialSSH(context.Background(), "root", host, port, SSHAuth{Password: "pw"}, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("DialSSH: %v", err)
	}
	defer client.Close()
	if accepted.Load() != 3 {
		t.Fatalf("connections = %d, want 3", accepted.Load())
	}
}

// A close after the endpoint identified itself as an SSH server is reported
// at once.
func TestDialSSHDoesNotRetryAnEndpointThatAnswered(t *testing.T) {
	fastConnectBackoff(t)
	host, port, accepted := flakyEntrance(t, fakeBackend(t), 1000, "SSH-2.0-OpenSSH_9.6\r\n")
	if _, _, err := DialSSH(context.Background(), "root", host, port, SSHAuth{Password: "pw"}, 5*time.Second, nil); err == nil {
		t.Fatal("DialSSH succeeded")
	}
	if accepted.Load() != 1 {
		t.Fatalf("connections = %d, want 1", accepted.Load())
	}
}

// A refused login is never tried again: that would count more failed logins
// against the target.
func TestDialSSHDoesNotRetryARefusedLogin(t *testing.T) {
	fastConnectBackoff(t)
	host, port, accepted := flakyEntrance(t, fakeBackend(t), 0, "")
	_, _, err := DialSSH(context.Background(), "root", host, port, SSHAuth{Password: "wrong"}, 5*time.Second, nil)
	if err == nil || accepted.Load() != 1 {
		t.Fatalf("err = %v, connections = %d", err, accepted.Load())
	}
}

// A canceled context stops the attempts.
func TestDialSSHStopsRetryingWhenCanceled(t *testing.T) {
	old := sshConnectBackoff
	sshConnectBackoff = func(int) time.Duration { return time.Hour }
	t.Cleanup(func() { sshConnectBackoff = old })
	host, port, accepted := flakyEntrance(t, fakeBackend(t), 1000, "")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := DialSSH(ctx, "root", host, port, SSHAuth{Password: "pw"}, 5*time.Second, nil); err == nil {
		t.Fatal("DialSSH succeeded")
	}
	if time.Since(start) > 5*time.Second || accepted.Load() != 1 {
		t.Fatalf("took %v over %d connections", time.Since(start), accepted.Load())
	}
}
