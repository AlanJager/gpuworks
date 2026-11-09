package sshutil

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Reconnect backoff; variables so tests can shorten them.
var (
	reconnectInitialDelay = 2 * time.Second
	reconnectMaxDelay     = 30 * time.Second
)

// session keeps one SSH connection alive for the life of a tunnel. A cloud
// SSH gateway can drop a long-lived session (RunPod's TCP proxy has, minutes
// into an engine start), and a tunnel that stays closed leaves the agent
// offline for the rest of the test. When the transport ends for any reason
// other than Close, the session dials again with backoff and hands the new
// client to attach, which restores what the tunnel needs on it. Every loss and
// recovery is reported through the tunnel's log function.
type session struct {
	name   string
	dial   func(context.Context) (*ssh.Client, error)
	attach func(*ssh.Client) error
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	client *ssh.Client
	closed bool
	logf   func(format string, args ...any)
}

func newSession(ctx context.Context, name string, dial func(context.Context) (*ssh.Client, error), attach func(*ssh.Client) error) (*session, error) {
	client, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	if err := attach(client); err != nil {
		client.Close()
		return nil, err
	}
	// ctx bounds the first dial only; the tunnel lives until Close, as it did
	// before it could reconnect. Redials keep ctx's values.
	life, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &session{name: name, dial: dial, attach: attach, ctx: life, cancel: cancel, client: client}
	go s.run(client)
	return s, nil
}

func (s *session) current() *ssh.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

func (s *session) setLogf(logf func(format string, args ...any)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logf = logf
}

// log never runs after close returns, so a test's Logf is not called once the
// test has finished.
func (s *session) log(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logf != nil && !s.closed {
		s.logf("%s: "+format, append([]any{s.name}, args...)...)
	}
}

func (s *session) run(client *ssh.Client) {
	for {
		current := client
		keepalive := make(chan error, 1)
		go func() { keepalive <- sendSSHKeepalives(s.ctx.Done(), current, sshKeepaliveInterval) }()
		waitErr := make(chan error, 1)
		go func() { waitErr <- current.Wait() }()
		var reason error
		select {
		case <-s.ctx.Done():
			return
		case err := <-keepalive:
			if err == nil {
				return
			}
			reason = fmt.Errorf("keepalive failed: %w", err)
			_ = current.Close()
		case err := <-waitErr:
			reason = fmt.Errorf("transport closed: %v", err)
		}
		lost := time.Now()
		s.log("SSH session lost (%v); reconnecting", reason)
		next, attempts := s.reconnect()
		if next == nil {
			return
		}
		s.log("SSH session restored after %s (%d attempts)", time.Since(lost).Round(time.Second), attempts)
		client = next
	}
}

func (s *session) reconnect() (*ssh.Client, int) {
	delay := reconnectInitialDelay
	for attempt := 1; ; attempt++ {
		select {
		case <-s.ctx.Done():
			return nil, attempt - 1
		case <-time.After(delay):
		}
		client, err := s.dial(s.ctx)
		if err == nil {
			if err = s.attach(client); err != nil {
				client.Close()
			}
		}
		if err == nil {
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				client.Close()
				return nil, attempt
			}
			s.client = client
			s.mu.Unlock()
			return client, attempt
		}
		s.log("reconnect attempt %d failed: %v", attempt, err)
		delay = min(delay*2, reconnectMaxDelay)
	}
}

// close stops reconnecting and closes the current connection. It returns
// false when the session was already closed.
func (s *session) close() bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	s.closed = true
	client := s.client
	s.mu.Unlock()
	s.cancel()
	_ = client.Close()
	return true
}

// RemoteForward exposes localAddr as an HTTP endpoint bound to loopback on the
// SSH server. The remote port stays the same across reconnects, because the
// agent on the far side was configured with this URL. Closing it tears down
// the listener and SSH transport.
type RemoteForward struct {
	bind    string
	session *session

	mu       sync.Mutex
	listener net.Listener
}

func ForwardServer(ctx context.Context, host string, port int, username string, privateKey []byte, localAddr string) (*RemoteForward, error) {
	return forwardServer(ctx, host, port, username, []ssh.AuthMethod{publicKeyAuth(privateKey)}, localAddr)
}

func ForwardServerPassword(ctx context.Context, host string, port int, username, password, localAddr string) (*RemoteForward, error) {
	return forwardServer(ctx, host, port, username, []ssh.AuthMethod{ssh.Password(password)}, localAddr)
}

func forwardServer(ctx context.Context, host string, port int, username string, auth []ssh.AuthMethod, localAddr string) (*RemoteForward, error) {
	target, err := forwardTarget(localAddr)
	if err != nil {
		return nil, err
	}
	f := &RemoteForward{bind: "127.0.0.1:0"}
	dial := func(ctx context.Context) (*ssh.Client, error) { return dialAuth(ctx, host, port, username, auth) }
	attach := func(client *ssh.Client) error {
		listener, err := client.Listen("tcp", f.bind)
		if err != nil {
			return fmt.Errorf("listen on SSH server %s: %w", f.bind, err)
		}
		f.mu.Lock()
		f.bind, f.listener = listener.Addr().String(), listener
		f.mu.Unlock()
		go serveForward(listener, target)
		return nil
	}
	f.session, err = newSession(ctx, fmt.Sprintf("ssh remote forward %s:%d", host, port), dial, attach)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// SetLogf reports SSH session losses and recoveries, for example to t.Logf.
func (f *RemoteForward) SetLogf(logf func(format string, args ...any)) { f.session.setLogf(logf) }

func (f *RemoteForward) URL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return "http://" + f.bind
}

func (f *RemoteForward) Close() error {
	if !f.session.close() {
		return nil
	}
	f.mu.Lock()
	listener := f.listener
	f.mu.Unlock()
	// The listener belongs to the SSH connection just closed; closing it only
	// releases local state, so its error carries nothing for the caller.
	_ = listener.Close()
	return nil
}

func serveForward(listener net.Listener, localAddr string) {
	for {
		remoteConn, err := listener.Accept()
		if err != nil {
			return
		}
		go proxy(remoteConn, localAddr)
	}
}

func proxy(remoteConn net.Conn, localAddr string) {
	localConn, err := net.DialTimeout("tcp", localAddr, 10*time.Second)
	if err != nil {
		_ = remoteConn.Close()
		return
	}
	proxyPair(remoteConn, localConn)
}

// ServerDialTunnel exposes a local listener whose connections are carried
// over SSH to the agent's loopback server_dial listener. The local address
// stays the same across SSH reconnects.
type ServerDialTunnel struct {
	listener net.Listener
	session  *session
}

func NewServerDialTunnel(ctx context.Context, host string, port int, username string, privateKey []byte, remoteAddr string) (*ServerDialTunnel, error) {
	return newServerDialTunnel(ctx, host, port, username, []ssh.AuthMethod{publicKeyAuth(privateKey)}, remoteAddr)
}

func NewServerDialTunnelPassword(ctx context.Context, host string, port int, username, password, remoteAddr string) (*ServerDialTunnel, error) {
	return newServerDialTunnel(ctx, host, port, username, []ssh.AuthMethod{ssh.Password(password)}, remoteAddr)
}

func newServerDialTunnel(ctx context.Context, host string, port int, username string, auth []ssh.AuthMethod, remoteAddr string) (*ServerDialTunnel, error) {
	if _, _, err := net.SplitHostPort(remoteAddr); err != nil {
		return nil, fmt.Errorf("invalid server_dial remote address %q: %w", remoteAddr, err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for server_dial tunnel: %w", err)
	}
	dial := func(ctx context.Context) (*ssh.Client, error) { return dialAuth(ctx, host, port, username, auth) }
	attach := func(*ssh.Client) error { return nil }
	s, err := newSession(ctx, fmt.Sprintf("ssh server_dial tunnel %s:%d", host, port), dial, attach)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	t := &ServerDialTunnel{listener: listener, session: s}
	go t.serve(remoteAddr)
	return t, nil
}

// SetLogf reports SSH session losses and recoveries, for example to t.Logf.
func (t *ServerDialTunnel) SetLogf(logf func(format string, args ...any)) { t.session.setLogf(logf) }

func (t *ServerDialTunnel) Addr() string { return t.listener.Addr().String() }

func (t *ServerDialTunnel) Close() error {
	if !t.session.close() {
		return nil
	}
	return t.listener.Close()
}

func (t *ServerDialTunnel) serve(remoteAddr string) {
	for {
		local, err := t.listener.Accept()
		if err != nil {
			return
		}
		go func() {
			remote, err := t.session.current().Dial("tcp", remoteAddr)
			if err != nil {
				_ = local.Close()
				return
			}
			proxyPair(local, remote)
		}()
	}
}
