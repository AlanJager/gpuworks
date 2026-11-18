// Package plugintest runs a plugin built with pluginsdk in process, against
// a fake server: it plays the server's side of both gwx/1 connections over
// real socketpairs, so a plugin's tests exercise the same HTTP/2 traffic as
// production without a server.
//
//	h := plugintest.Start(t, cfg)
//	resp, err := h.Initialize(h.Request())
//	push := h.WaitPush(extapi.KindBanners, 5*time.Second)
package plugintest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
)

// Harness is one plugin run against the fake server.
type Harness struct {
	t      testing.TB
	cfg    pluginsdk.Config
	client *http.Client
	cc     *http.ClientConn
	host   *fakeHost
	srv    *http.Server
	conns  []net.Conn
	done   chan error
	// cursor is, per kind, the index after the last push WaitPush
	// returned; guarded by host.mu.
	cursor map[string]int
}

// Start runs cfg's plugin in a goroutine on two socketpairs. It does not
// initialize the plugin; call Initialize. The harness closes at the end of
// the test.
func Start(t testing.TB, cfg pluginsdk.Config) *Harness {
	t.Helper()
	coreServe, pluginServe, err := socketPair()
	if err != nil {
		t.Fatal(err)
	}
	coreHost, pluginHost, err := socketPair()
	if err != nil {
		t.Fatal(err)
	}
	h := &Harness{t: t, cfg: cfg, host: newFakeHost(), done: make(chan error, 1), cursor: map[string]int{},
		conns: []net.Conn{coreServe, coreHost}}
	go func() { h.done <- pluginsdk.Serve(context.Background(), cfg, pluginServe, pluginHost) }()

	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	h.srv = &http.Server{Handler: h.host, Protocols: &p, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = h.srv.Serve(&oneConnListener{conn: coreHost, closed: make(chan struct{})}) }()

	var dialed sync.Once
	tr := &http.Transport{Protocols: &p, DialContext: func(context.Context, string, string) (net.Conn, error) {
		var c net.Conn
		dialed.Do(func() { c = coreServe })
		if c == nil {
			return nil, errors.New("plugintest: the plugin connection is already in use")
		}
		return c, nil
	}}
	if h.cc, err = tr.NewClientConn(context.Background(), "http", "gwx-plugin:80"); err == nil {
		h.client = &http.Client{Transport: h.cc}
	} else {
		// The plugin hung up at once (a config it refuses); its calls fail
		// with this, and Wait returns why.
		h.client = &http.Client{Transport: failingTransport{err}}
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// Request is a valid initialize request for cfg's package: this server's
// offer of every table kind at every schema extapi defines plus routes@1
// and node/host identity reads, clock, own-state and derived-key calls. It offers the
// tables the fake host currently holds, but grants no permission or role;
// tests set Plugin.PermissionsGranted or Plugin.RolesGranted as an
// administrator's approval or a certificate does.
func (h *Harness) Request() *extapi.HandshakeRequest {
	offer := map[string]extapi.Range{extapi.CapRoutes: {Min: 1, Max: 1},
		extapi.CapHostNodeIdentity: {Min: 1, Max: 1}, extapi.CapHostNodes: {Min: 1, Max: 1},
		extapi.CapHostHostIdentity: {Min: 1, Max: 1}, extapi.CapHostClock: {Min: 1, Max: 1}, extapi.CapHostStateRead: {Min: 1, Max: 1}, extapi.CapHostDeriveKey: {Min: 1, Max: 1}}
	for _, kind := range extapi.TableKinds() {
		s := extapi.TableSchemas(kind)
		offer[extapi.TableCapability(kind)] = extapi.Range{Min: s[0], Max: s[len(s)-1]}
	}
	now := extapi.NewTime(time.Now())
	return &extapi.HandshakeRequest{
		Protocol: extapi.Range{Min: extapi.ProtocolMajor, Max: extapi.ProtocolMajor},
		Core: extapi.CoreInfo{Version: "v0.0.0-plugintest", InstallID: "plugintest_install", OS: "linux", Arch: "amd64",
			HARole: extapi.HARoleSingle, Offer: offer},
		Plugin:       extapi.PluginGrant{ID: h.cfg.ID, Version: h.cfg.Version},
		Held:         h.Held(),
		Clock:        extapi.Clock{Wall: now, ExtClock: now},
		PrincipalKey: h.PrincipalKey(),
	}
}

// PrincipalKey is the fixed principal key Request hands the plugin, and
// the key Sign signs with.
func (h *Harness) PrincipalKey() []byte { return bytes.Repeat([]byte{7}, extapi.PrincipalKeyBytes) }

// Principal is a principal for a route request the server matched to
// action and route (the manifest's pattern): an administrator signed in
// with a session, expiring after the default route timeout. Change its
// fields for other callers before Sign.
func (h *Harness) Principal(action, route string) *extapi.Principal {
	return &extapi.Principal{Kind: extapi.PrincipalSession, UserID: 1, Username: "admin", PlatformAdmin: true,
		LocalAdmin: true, Roles: []string{"admin"}, RequestID: "plugintest-request", RouteAction: action, Route: route,
		Exp: extapi.PrincipalExpiry(time.Now(), 0)}
}

// Sign sets the principal headers on req as the server does for a route
// request: p and access signed under PrincipalKey. It fails the test when
// p or access is invalid. Send req with Do to a path under /r/.
func (h *Harness) Sign(req *http.Request, p *extapi.Principal, access extapi.FeatureAccess) {
	h.t.Helper()
	hdr, mac, err := extapi.SignPrincipal(h.PrincipalKey(), p, access.String())
	if err != nil {
		h.t.Fatalf("plugintest: sign the principal: %v", err)
	}
	req.Header.Set(extapi.HeaderPrincipal, hdr)
	req.Header.Set(extapi.HeaderPrincipalMac, mac)
	req.Header.Set(extapi.HeaderFeatureAccess, access.String())
}

// Initialize sends req; a refusal is a *pluginsdk.Error.
func (h *Harness) Initialize(req *extapi.HandshakeRequest) (*extapi.HandshakeResponse, error) {
	body, err := extapi.Marshal(req)
	if err != nil {
		return nil, err
	}
	raw, err := h.call(http.MethodPost, extapi.PathInitialize, body)
	if err != nil {
		return nil, err
	}
	resp, err := extapi.DecodeHandshakeResponse(raw)
	if err != nil {
		return nil, err
	}
	h.host.acceptInit(req, resp)
	return resp, nil
}

// Health probes the plugin.
func (h *Harness) Health() (*extapi.Health, error) {
	raw, err := h.call(http.MethodGet, extapi.PathHealth, nil)
	if err != nil {
		return nil, err
	}
	return extapi.DecodeHealth(raw)
}

// Shutdown asks the plugin to stop and returns what its Serve returned.
func (h *Harness) Shutdown() error {
	if _, err := h.call(http.MethodPost, extapi.PathShutdown, nil); err != nil {
		return err
	}
	return h.Wait(10 * time.Second)
}

// Do sends req to the plugin (a route or a service); only its path, method,
// headers and body matter.
func (h *Harness) Do(req *http.Request) (*http.Response, error) {
	req.URL.Scheme, req.URL.Host, req.Host = "http", "gwx-plugin:80", ""
	return h.client.Do(req)
}

func (h *Harness) call(method, path string, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://gwx-plugin:80"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set(extapi.HeaderDeadline, fmt.Sprint(time.Now().Add(10*time.Second).UnixMilli()))
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		w, derr := extapi.DecodeWireError(raw)
		if derr != nil {
			return nil, fmt.Errorf("plugintest: status %d with a body that is not an error object: %w", resp.StatusCode, derr)
		}
		return nil, &pluginsdk.Error{Status: resp.StatusCode, Wire: *w}
	}
	return raw, nil
}

// Pushes returns every push received so far, in order.
func (h *Harness) Pushes() []Push {
	h.host.mu.Lock()
	defer h.host.mu.Unlock()
	return append([]Push(nil), h.host.pushes...)
}

// WaitPush returns the first push of kind that WaitPush has not returned
// before, waiting for it, and fails the test when none arrives within
// timeout.
func (h *Harness) WaitPush(kind string, timeout time.Duration) Push {
	h.t.Helper()
	deadline := time.After(timeout)
	for {
		h.host.mu.Lock()
		for i := h.cursor[kind]; i < len(h.host.pushes); i++ {
			if p := h.host.pushes[i]; p.Envelope.Kind == kind {
				h.cursor[kind] = i + 1
				h.host.mu.Unlock()
				return p
			}
		}
		changed := h.host.changed
		h.host.mu.Unlock()
		select {
		case <-changed:
		case <-deadline:
			h.t.Fatalf("plugintest: no push of %s within %s", kind, timeout)
		}
	}
}

// Held returns the tables the fake host holds, sorted by kind, as a
// restarted server would send them in initialize.
func (h *Harness) Held() []extapi.HeldState {
	h.host.mu.Lock()
	defer h.host.mu.Unlock()
	out := make([]extapi.HeldState, 0, len(h.host.held))
	for _, s := range h.host.held {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// SetHeld replaces what the fake host holds, for example to start a plugin
// against a server that kept state from an earlier run.
func (h *Harness) SetHeld(held ...extapi.HeldState) {
	h.host.mu.Lock()
	defer h.host.mu.Unlock()
	h.host.held = map[string]extapi.HeldState{}
	for _, s := range held {
		h.host.held[s.Kind] = s
	}
}

// RefuseNextPush makes the fake host refuse the next push of kind with e,
// for the refusals that depend on the server's state: for example
// PLUGIN_HOST_RATE_LIMITED (reason busy, with retry_after_ms) or
// PLUGIN_STATE_NOT_PERMITTED (reason permission_denied).
func (h *Harness) RefuseNextPush(kind string, e extapi.WireError) {
	h.host.mu.Lock()
	defer h.host.mu.Unlock()
	h.host.refuse[kind] = e
}

// Wait returns what the plugin's Serve returned, or an error when it is
// still running after timeout.
func (h *Harness) Wait(timeout time.Duration) error {
	select {
	case err := <-h.done:
		h.done <- err // later calls see the same result
		return err
	case <-time.After(timeout):
		return fmt.Errorf("plugintest: the plugin still runs after %s", timeout)
	}
}

// Close closes the server's ends of both connections, as a dying server
// does, and returns what the plugin's Serve returned.
func (h *Harness) Close() error {
	if h.cc != nil {
		_ = h.cc.Close()
	}
	_ = h.srv.Close()
	for _, c := range h.conns {
		_ = c.Close()
	}
	return h.Wait(10 * time.Second)
}

// oneConnListener hands http.Server one connection, then blocks until the
// server closes it.
type oneConnListener struct {
	conn   net.Conn
	once   sync.Once
	closed chan struct{}
	close  sync.Once
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

type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }
