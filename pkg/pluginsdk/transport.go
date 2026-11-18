package pluginsdk

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// h2c is the protocol set of both gwx/1 connections: HTTP/2 with prior
// knowledge, no TLS and no HTTP/1.
func h2c() *http.Protocols {
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	return &p
}

// connsFromEnv opens the two descriptors the server handed the plugin.
func connsFromEnv() (serve, host net.Conn, err error) {
	if v := os.Getenv(extapi.EnvProtocol); v != strconv.Itoa(extapi.ProtocolMajor) {
		return nil, nil, &extapi.Error{Code: extapi.ErrorProtocolUnsupported,
			Message: fmt.Sprintf("%s is %q; this plugin speaks gwx/%d", extapi.EnvProtocol, v, extapi.ProtocolMajor),
			Params:  map[string]any{"min": 0, "max": 0, "supported": extapi.ProtocolMajor}}
	}
	serve, err = fdConn(extapi.EnvServeFD)
	if err != nil {
		return nil, nil, err
	}
	host, err = fdConn(extapi.EnvHostFD)
	if err != nil {
		_ = serve.Close()
		return nil, nil, err
	}
	return serve, host, nil
}

func fdConn(env string) (net.Conn, error) {
	n, err := strconv.Atoi(os.Getenv(env))
	if err != nil || n < 3 {
		return nil, fmt.Errorf("pluginsdk: %s=%q is not an inherited descriptor; the plugin runs only under the server", env, os.Getenv(env))
	}
	f := os.NewFile(uintptr(n), env)
	if f == nil {
		return nil, fmt.Errorf("pluginsdk: %s=%d is not open", env, n)
	}
	defer f.Close()
	c, err := net.FileConn(f)
	if err != nil {
		return nil, fmt.Errorf("pluginsdk: %s=%d is not a socket: %w", env, n, err)
	}
	return c, nil
}

// oneConnListener hands http.Server exactly one connection, then blocks
// until it is closed.
type oneConnListener struct {
	conn   net.Conn
	once   sync.Once
	closed chan struct{}
	close  sync.Once
}

func newOneConnListener(c net.Conn) *oneConnListener {
	return &oneConnListener{conn: c, closed: make(chan struct{})}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.conn })
	if c != nil {
		return c, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error {
	l.close.Do(func() { close(l.closed) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

// hostClient is an HTTP/2 client on the one host connection. The transport
// dials only that connection, and only once.
func hostClient(ctx context.Context, conn net.Conn) (*http.Client, func() error, error) {
	var dialed sync.Once
	tr := &http.Transport{
		Protocols: h2c(),
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			var c net.Conn
			dialed.Do(func() { c = conn })
			if c == nil {
				return nil, errors.New("pluginsdk: the host connection is already in use")
			}
			return c, nil
		},
	}
	cc, err := tr.NewClientConn(ctx, "http", hostAuthority)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("pluginsdk: opening the host connection: %w", err)
	}
	return &http.Client{Transport: cc}, cc.Close, nil
}

// hostAuthority is the authority of every host API request; the connection
// decides who answers, so it names nothing.
const hostAuthority = "gwx-host:80"

// setDeadline copies ctx's deadline into the gwx/1 deadline header.
func setDeadline(ctx context.Context, h http.Header) {
	if d, ok := ctx.Deadline(); ok {
		h.Set(extapi.HeaderDeadline, strconv.FormatInt(d.UnixMilli(), 10))
	}
}

// withDeadline bounds a served call by the deadline the caller sent.
func withDeadline(r *http.Request) (*http.Request, context.CancelFunc) {
	ms, err := strconv.ParseInt(r.Header.Get(extapi.HeaderDeadline), 10, 64)
	if err != nil || ms <= 0 {
		return r, func() {}
	}
	ctx, cancel := context.WithDeadline(r.Context(), time.UnixMilli(ms))
	return r.WithContext(ctx), cancel
}

// getppid is replaced in tests.
var getppid = os.Getppid

// watchParent returns a channel closed once the parent process is gone
// (the plugin was reparented), polled every interval. Linux also kills the
// plugin with a parent-death signal; macOS has none, so this is how a
// plugin there notices that the server died.
func watchParent(ctx context.Context, interval time.Duration) <-chan struct{} {
	gone := make(chan struct{})
	parent := getppid()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if getppid() != parent {
					close(gone)
					return
				}
			}
		}
	}()
	return gone
}
