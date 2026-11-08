package sshutil

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeSSHD behaves like OpenSSH for what the tunnels use: a remote forward
// (tcpip-forward) lives as long as the session that asked for it, direct-tcpip
// dials the requested address, and keepalive@openssh.com is answered with a
// failure reply. kill drops every session, as a cloud SSH gateway does.
type fakeSSHD struct {
	t        *testing.T
	listener net.Listener
	config   *ssh.ServerConfig

	mu          sync.Mutex
	sessions    []*ssh.ServerConn
	logins      int
	denyForward int
}

func newFakeSSHD(t *testing.T) *fakeSSHD {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if meta.User() != "root" || string(password) != "secret" {
			return nil, fmt.Errorf("denied")
		}
		return nil, nil
	}}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeSSHD{t: t, listener: listener, config: config}
	t.Cleanup(func() { _ = listener.Close(); d.kill() })
	go d.accept()
	return d
}

func (d *fakeSSHD) port() int { return d.listener.Addr().(*net.TCPAddr).Port }

func (d *fakeSSHD) loginCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.logins
}

// denyForwards refuses the next n tcpip-forward requests, like sshd while the
// dropped session still holds the port.
func (d *fakeSSHD) denyForwards(n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.denyForward = n
}

func (d *fakeSSHD) kill() {
	d.mu.Lock()
	sessions := d.sessions
	d.sessions = nil
	d.mu.Unlock()
	for _, s := range sessions {
		_ = s.Close()
	}
}

func (d *fakeSSHD) accept() {
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			return
		}
		go d.serve(conn)
	}
}

func (d *fakeSSHD) serve(conn net.Conn) {
	server, channels, requests, err := ssh.NewServerConn(conn, d.config)
	if err != nil {
		_ = conn.Close()
		return
	}
	d.mu.Lock()
	d.sessions = append(d.sessions, server)
	d.logins++
	d.mu.Unlock()
	var forwards []net.Listener
	var forwardsMu sync.Mutex
	defer func() {
		forwardsMu.Lock()
		defer forwardsMu.Unlock()
		for _, l := range forwards {
			_ = l.Close()
		}
	}()
	go func() {
		for request := range requests {
			switch request.Type {
			case "tcpip-forward":
				var bind struct {
					Addr string
					Port uint32
				}
				_ = ssh.Unmarshal(request.Payload, &bind)
				d.mu.Lock()
				deny := d.denyForward > 0
				if deny {
					d.denyForward--
				}
				d.mu.Unlock()
				if deny {
					_ = request.Reply(false, nil)
					continue
				}
				l, err := net.Listen("tcp", net.JoinHostPort(bind.Addr, fmt.Sprint(bind.Port)))
				if err != nil {
					_ = request.Reply(false, nil)
					continue
				}
				forwardsMu.Lock()
				forwards = append(forwards, l)
				forwardsMu.Unlock()
				port := uint32(l.Addr().(*net.TCPAddr).Port)
				_ = request.Reply(true, ssh.Marshal(struct{ Port uint32 }{port}))
				go d.forward(server, l, bind.Addr, port)
			default:
				_ = request.Reply(false, nil)
			}
		}
	}()
	for channel := range channels {
		if channel.ChannelType() != "direct-tcpip" {
			_ = channel.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		var target struct {
			Addr       string
			Port       uint32
			OriginAddr string
			OriginPort uint32
		}
		_ = ssh.Unmarshal(channel.ExtraData(), &target)
		remote, err := net.Dial("tcp", net.JoinHostPort(target.Addr, fmt.Sprint(target.Port)))
		if err != nil {
			_ = channel.Reject(ssh.ConnectionFailed, err.Error())
			continue
		}
		ch, reqs, err := channel.Accept()
		if err != nil {
			_ = remote.Close()
			continue
		}
		go ssh.DiscardRequests(reqs)
		go pipe(ch, remote)
	}
}

func (d *fakeSSHD) forward(server *ssh.ServerConn, l net.Listener, addr string, port uint32) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		origin := conn.RemoteAddr().(*net.TCPAddr)
		ch, reqs, err := server.OpenChannel("forwarded-tcpip", ssh.Marshal(struct {
			Addr       string
			Port       uint32
			OriginAddr string
			OriginPort uint32
		}{addr, port, origin.IP.String(), uint32(origin.Port)}))
		if err != nil {
			_ = conn.Close()
			continue
		}
		go ssh.DiscardRequests(reqs)
		go pipe(ch, conn)
	}
}

func pipe(a io.ReadWriteCloser, b io.ReadWriteCloser) {
	defer a.Close()
	defer b.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}

// logRecorder collects tunnel log lines and fails the test if one arrives
// after it was detached.
type logRecorder struct {
	mu       sync.Mutex
	lines    []string
	detached bool
	t        *testing.T
}

func (r *logRecorder) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.detached {
		r.t.Errorf("tunnel logged after detach: "+format, args...)
	}
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *logRecorder) has(part string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range r.lines {
		if strings.Contains(line, part) {
			return true
		}
	}
	return false
}

func fastReconnect(t *testing.T) {
	initial, maximum := reconnectInitialDelay, reconnectMaxDelay
	reconnectInitialDelay, reconnectMaxDelay = 20*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { reconnectInitialDelay, reconnectMaxDelay = initial, maximum })
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func get(url string) (string, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

// A dropped SSH session must not leave the agent's server URL dead: the
// forward redials and listens on the same remote port again, retrying while
// the server still refuses the port, and reports the loss and the recovery.
func TestRemoteForwardReconnectsOnTheSamePort(t *testing.T) {
	fastReconnect(t)
	sshd := newFakeSSHD(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "control plane") }))
	defer backend.Close()
	forward, err := ForwardServerPassword(context.Background(), "127.0.0.1", sshd.port(), "root", "secret", backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	logs := &logRecorder{t: t}
	forward.SetLogf(logs.logf)
	url := forward.URL()
	if body, err := get(url); err != nil || body != "control plane" {
		t.Fatalf("forward before drop: body=%q err=%v", body, err)
	}

	sshd.denyForwards(2)
	sshd.kill()
	eventually(t, "the remote port to close with the dropped session", func() bool { _, err := get(url); return err != nil })
	eventually(t, "the forward to serve again", func() bool { body, err := get(url); return err == nil && body == "control plane" })
	if forward.URL() != url {
		t.Fatalf("forward moved from %s to %s; the agent still dials the old URL", url, forward.URL())
	}
	for _, want := range []string{"SSH session lost", "reconnect attempt 1 failed", "reconnect attempt 2 failed", "SSH session restored"} {
		if !logs.has(want) {
			t.Fatalf("log lacks %q: %q", want, logs.lines)
		}
	}
	if logins := sshd.loginCount(); logins != 4 {
		t.Fatalf("logins=%d, want 4 (first, two refused forwards, success)", logins)
	}

	if err := forward.Close(); err != nil {
		t.Fatal(err)
	}
	logs.mu.Lock()
	logs.detached = true
	logs.mu.Unlock()
	sshd.kill()
	time.Sleep(300 * time.Millisecond)
	if logins := sshd.loginCount(); logins != 4 {
		t.Fatalf("a closed forward dialed again: logins=%d", logins)
	}
	if _, err := get(url); err == nil {
		t.Fatal("closed forward still serves")
	}
}

// A detached logger is never called, so a finished test's Logf is safe even
// when its forward stays open (a kept failed cloud instance).
func TestRemoteForwardDetachedLoggerIsSilent(t *testing.T) {
	fastReconnect(t)
	sshd := newFakeSSHD(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer backend.Close()
	forward, err := ForwardServerPassword(context.Background(), "127.0.0.1", sshd.port(), "root", "secret", backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer forward.Close()
	logs := &logRecorder{t: t, detached: true}
	forward.SetLogf(logs.logf)
	forward.SetLogf(nil)
	sshd.kill()
	eventually(t, "the forward to serve again", func() bool { body, err := get(forward.URL()); return err == nil && body == "ok" })
}

// The server_dial tunnel keeps its local address and carries new connections
// over the redialed session.
func TestServerDialTunnelReconnects(t *testing.T) {
	fastReconnect(t)
	sshd := newFakeSSHD(t)
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	tunnel, err := NewServerDialTunnelPassword(context.Background(), "127.0.0.1", sshd.port(), "root", "secret", echo.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.Close()
	logs := &logRecorder{t: t}
	tunnel.SetLogf(logs.logf)
	roundTrip := func() bool {
		conn, err := net.DialTimeout("tcp", tunnel.Addr(), time.Second)
		if err != nil {
			return false
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		if _, err := conn.Write([]byte("ping")); err != nil {
			return false
		}
		buf := make([]byte, 4)
		_, err = io.ReadFull(conn, buf)
		return err == nil && string(buf) == "ping"
	}
	if !roundTrip() {
		t.Fatal("tunnel before drop does not carry data")
	}
	addr := tunnel.Addr()
	sshd.kill()
	eventually(t, "the tunnel to carry data again", roundTrip)
	if tunnel.Addr() != addr {
		t.Fatalf("tunnel address moved from %s to %s", addr, tunnel.Addr())
	}
	if !logs.has("SSH session lost") || !logs.has("SSH session restored") {
		t.Fatalf("log lacks loss or recovery: %q", logs.lines)
	}
}
