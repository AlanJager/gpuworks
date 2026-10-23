package provision

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshConnectAttempts bounds the logins tried against an endpoint that
// closes connections before it sends its SSH identification. sshd does that
// past MaxStartups: with 10 (by default) connections still authenticating it
// drops new ones at random, writing "Exceeded MaxStartups" instead. A host
// whose SSH port is open to the internet stays there under scanners'
// password guessing: a CompShare container's forwarded SSH port
// (…podtcp.compshare.cn) dropped 30–50% of connections, one apart from the
// next (observed 2026-10-23), so one drop says nothing about the next
// connection.
const sshConnectAttempts = 8

// sshConnectBackoff is the pause before the next attempt.
var sshConnectBackoff = func(attempt int) time.Duration {
	return min(time.Duration(attempt)*250*time.Millisecond, time.Second)
}

// connect opens an SSH client, trying again while the endpoint closes the
// connection before sending its identification. Such a close happens before
// authentication, so trying again cannot count a failed login against the
// target.
func connect(ctx context.Context, user, host string, port int, methods []ssh.AuthMethod, timeout time.Duration, hostKeys HostKeyStore) (*ssh.Client, error) {
	for attempt := 1; ; attempt++ {
		client, err := connectOnce(ctx, user, host, port, methods, timeout, hostKeys)
		var closed *closedBeforeIdentError
		if err == nil || !errors.As(err, &closed) || attempt == sshConnectAttempts {
			if closed != nil && attempt > 1 {
				err = fmt.Errorf("%w (the endpoint closed all %d connections before identifying itself: %s)", err, attempt, closed.said())
			}
			return client, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(sshConnectBackoff(attempt)):
		}
	}
}

// identCapturingConn keeps the first bytes the endpoint sent, enough to
// tell whether it identified itself as an SSH server.
type identCapturingConn struct {
	net.Conn
	mu    sync.Mutex
	first []byte
}

const identCaptureBytes = 512

func (c *identCapturingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	if room := identCaptureBytes - len(c.first); room > 0 {
		c.first = append(c.first, p[:min(n, room)]...)
	}
	c.mu.Unlock()
	return n, err
}

// identified reports whether the endpoint sent an SSH identification line;
// RFC 4253 lets other lines come before it.
func (c *identCapturingConn) identified() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.HasPrefix(c.first, []byte("SSH-")) || bytes.Contains(c.first, []byte("\nSSH-"))
}

func (c *identCapturingConn) sent() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.TrimSpace(string(c.first))
}

// closedBeforeIdentError is a handshake the endpoint ended before it
// identified itself as an SSH server.
type closedBeforeIdentError struct {
	err  error
	sent string
}

func (e *closedBeforeIdentError) Error() string { return e.err.Error() }
func (e *closedBeforeIdentError) Unwrap() error { return e.err }

func (e *closedBeforeIdentError) said() string {
	if e.sent == "" {
		return "nothing"
	}
	return strconv.Quote(e.sent)
}

func handshakeError(conn *identCapturingConn, err error) error {
	wrapped := fmt.Errorf("ssh handshake: %w", err)
	if !conn.identified() && (errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET)) {
		return &closedBeforeIdentError{err: wrapped, sent: conn.sent()}
	}
	return wrapped
}
