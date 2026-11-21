package testconn

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

const (
	networkTimeout = 5 * time.Second
	probeTimeout   = 10 * time.Second
	clockSkewLimit = 5 * time.Minute
)

// networkChecks are the checks that need an endpoint, in report order.
var networkChecks = []string{CheckDNSResolve, CheckTCPConnect, CheckTLSHandshake, CheckProxyDetect, CheckClockSkew}

// network runs the generic network layer: DNS, TCP, TLS, proxy and clock
// skew. What the server itself must reach is the route's first hop: the
// endpoint, or the proxy its requests go through.
func (st *runState) network(ctx context.Context, u *url.URL) {
	r := st.r
	rt := r.routeFor(u)
	st.step(ctx, CheckDNSResolve, nil, func(ctx context.Context) (map[string]any, error) {
		return r.checkDNS(ctx, rt)
	})
	st.step(ctx, CheckTCPConnect, []string{CheckDNSResolve}, func(ctx context.Context) (map[string]any, error) {
		return r.checkTCP(ctx, rt)
	})
	st.step(ctx, CheckTLSHandshake, []string{CheckTCPConnect}, func(ctx context.Context) (map[string]any, error) {
		return r.checkTLS(ctx, rt)
	})
	st.step(ctx, CheckProxyDetect, nil, func(context.Context) (map[string]any, error) {
		return r.checkProxy(rt)
	})
	st.step(ctx, CheckClockSkew, []string{CheckTCPConnect}, func(ctx context.Context) (map[string]any, error) {
		return r.checkClockSkew(ctx, u)
	})
}

func (r *Runner) checkDNS(ctx context.Context, rt route) (map[string]any, error) {
	host := rt.hop.Hostname()
	if net.ParseIP(host) != nil {
		if rt.viaProxy() {
			return map[string]any{"target": "proxy"}, nil
		}
		return map[string]any{"ips": []string{host}, "literal": true}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, networkTimeout)
	defer cancel()
	ips, err := r.LookupHost(ctx, host)
	if err != nil {
		if rt.viaProxy() {
			return nil, proxyFailure(err)
		}
		return nil, &Failure{Code: messages.ErrorProviderDNSFailed, Hint: "dns",
			Detail: map[string]any{"host": host}, Err: err}
	}
	if rt.viaProxy() {
		// The proxy's address comes from a proxy variable, whose value stays
		// off the wire.
		return map[string]any{"target": "proxy"}, nil
	}
	return map[string]any{"ips": ips}, nil
}

// dialAddress is the host:port a URL is dialed at, with the scheme's default
// port when the URL names none.
func dialAddress(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

func classifyDial(err error) *Failure {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return &Failure{Code: messages.ErrorProviderTCPRefused, Hint: "tcp_refused", Err: err}
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return &Failure{Code: messages.ErrorProviderTCPUnreachable, Hint: "tcp_unreachable", Err: err}
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		return &Failure{Code: messages.ErrorProviderTCPTimeout, Hint: "tcp_timeout", Err: err}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &Failure{Code: messages.ErrorProviderTCPTimeout, Hint: "tcp_timeout", Err: err}
	}
	return &Failure{Code: messages.ErrorProviderTCPUnreachable, Hint: "tcp_unreachable", Err: err}
}

func (r *Runner) checkTCP(ctx context.Context, rt route) (map[string]any, error) {
	addr := dialAddress(rt.hop)
	ctx, cancel := context.WithTimeout(ctx, networkTimeout)
	defer cancel()
	started := r.Now()
	conn, err := r.Dial(ctx, "tcp", addr)
	if err != nil {
		if rt.viaProxy() {
			return nil, proxyFailure(err)
		}
		return nil, classifyDial(err)
	}
	_ = conn.Close()
	rtt := r.Now().Sub(started).Milliseconds()
	if rt.viaProxy() {
		return map[string]any{"target": "proxy", "rtt_ms": rtt}, nil
	}
	return map[string]any{"address": addr, "rtt_ms": rtt}, nil
}

// tlsConfig is the runner's TLS client configuration for one server name,
// verifying certificates against the runner's clock.
func (r *Runner) tlsConfig(serverName string) *tls.Config {
	cfg := &tls.Config{}
	if r.TLSConfig != nil {
		cfg = r.TLSConfig.Clone()
	}
	cfg.ServerName = serverName
	if cfg.Time == nil {
		cfg.Time = r.Now
	}
	return cfg
}

// checkTLS dials through the runner's dialer and handshakes over that
// connection, so the handshake takes the same path as tcp_connect.
func (r *Runner) checkTLS(ctx context.Context, rt route) (map[string]any, error) {
	if rt.viaProxy() {
		// The server's own connection ends at the proxy; the endpoint's
		// certificate is the proxy's to reach.
		return nil, &skipCheck{hint: HintViaProxy}
	}
	u := rt.hop
	if u.Scheme != "https" {
		return map[string]any{"mode": "plain_http"}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, networkTimeout)
	defer cancel()
	raw, err := r.Dial(ctx, "tcp", dialAddress(u))
	if err != nil {
		return nil, &Failure{Code: messages.ErrorProviderTLSFailed, Hint: "tls", Err: err}
	}
	conn := tls.Client(raw, r.tlsConfig(u.Hostname()))
	defer conn.Close()
	if err := conn.HandshakeContext(ctx); err != nil {
		return nil, &Failure{Code: messages.ErrorProviderTLSFailed, Hint: "tls", Err: err}
	}
	detail := map[string]any{"server_name": u.Hostname()}
	if certs := conn.ConnectionState().PeerCertificates; len(certs) > 0 {
		detail["cert_days_left"] = int(certs[0].NotAfter.Sub(r.Now()).Hours() / 24)
	}
	return detail, nil
}

// httpClient is the client the clock-skew request uses: the injected one, or
// one built over the runner's proxy decision, dialer and TLS configuration.
// The returned func releases what the built client holds.
func (r *Runner) httpClient() (*http.Client, func()) {
	if r.HTTPClient != nil {
		return r.HTTPClient, func() {}
	}
	transport := &http.Transport{
		Proxy:               r.proxyFor,
		DialContext:         r.Dial,
		TLSClientConfig:     r.tlsConfig(""),
		TLSHandshakeTimeout: networkTimeout,
		DisableKeepAlives:   true,
		// A proxy that answers CONNECT with a refusal fails the request with
		// the bare status text; name it as the proxy's refusal.
		OnProxyConnectResponse: func(_ context.Context, _ *url.URL, _ *http.Request, resp *http.Response) error {
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("%w: status %d", errProxyRefused, resp.StatusCode)
			}
			return nil
		},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   probeTimeout,
		// The endpoint's own Date header is the one compared; a redirect
		// would answer with another host's.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return client, transport.CloseIdleConnections
}

// checkClockSkew compares the endpoint's Date header with the local clock. An
// unsigned request is fine: even a 403 response carries Date.
func (r *Runner) checkClockSkew(ctx context.Context, u *url.URL) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	client, release := r.httpClient()
	defer release()
	resp, err := client.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.Is(err, errProxyConfig) || errors.Is(err, errProxyRefused) || (errors.As(err, &op) && op.Op == "proxyconnect") {
			return nil, proxyFailure(err)
		}
		return nil, &Failure{Code: messages.ErrorProviderHTTPUnreachable, Hint: "http", Err: err}
	}
	defer resp.Body.Close()
	date := resp.Header.Get("Date")
	if date == "" {
		return map[string]any{"date_header": "absent"}, nil
	}
	when, err := http.ParseTime(date)
	if err != nil {
		return map[string]any{"date_header": "unparseable"}, nil
	}
	skew := r.Now().Sub(when).Abs()
	if skew > clockSkewLimit {
		return nil, &Failure{Code: messages.ErrorProviderClockSkew, Hint: "clock_skew",
			Detail: map[string]any{"skew_seconds": int(skew.Seconds())},
			Err:    fmt.Errorf("clock skew %s", skew)}
	}
	return map[string]any{"skew_seconds": int(skew.Seconds())}, nil
}
