package conn

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync"
)

// transportObservation follows one actual dial. Data attempts deliberately do
// not overwrite the control connection's phase facts.
type transportObservation struct {
	conn      *Conn
	control   bool
	mu        sync.Mutex
	phase     string
	serverTLS bool
}

func (o *transportObservation) start(stage string) {
	o.mu.Lock()
	o.phase = stage
	o.mu.Unlock()
	if o.control {
		o.conn.beginConnectionPhase(stage, false)
	}
}
func (o *transportObservation) done(stage string, err error) {
	if o.control {
		o.conn.endConnectionPhase(stage, err)
	}
}
func (o *transportObservation) current() string { o.mu.Lock(); defer o.mu.Unlock(); return o.phase }

func (c *Conn) observeHTTPClient(client *http.Client, control bool) (*http.Client, *transportObservation) {
	copyClient := *client
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	observer := &transportObservation{conn: c, control: control, phase: "dial"}
	if transport, ok := base.(*http.Transport); ok {
		clone := transport.Clone()
		originalProxy := clone.Proxy
		clone.Proxy = func(req *http.Request) (*url.URL, error) {
			var proxy *url.URL
			var err error
			if originalProxy != nil {
				proxy, err = originalProxy(req)
			}
			observer.mu.Lock()
			observer.serverTLS = req.URL.Scheme == "https" && proxy == nil
			observer.mu.Unlock()
			if control {
				selection := "direct"
				if err != nil {
					selection = "unknown"
				} else if proxy != nil {
					selection = "environment_proxy"
				}
				c.observationFact("proxy", selection)
				c.observationFact("proxy_url", safeProxyURL(proxy, c.creds.NodeToken))
				if proxy == nil || req.URL.Scheme != "https" {
					c.observationPhaseState("proxy_connect", "not_applicable")
				}
				if req.URL.Scheme != "https" {
					c.observationPhaseState("tls", "not_applicable")
				}
			}
			return proxy, err
		}
		previous := clone.OnProxyConnectResponse
		clone.OnProxyConnectResponse = func(ctx context.Context, proxy *url.URL, req *http.Request, response *http.Response) error {
			var err error
			if previous != nil {
				err = previous(ctx, proxy, req, response)
			}
			observer.mu.Lock()
			observer.phase = "proxy_connect"
			observer.serverTLS = response.StatusCode == http.StatusOK && err == nil
			observer.mu.Unlock()
			if control {
				// There is no reliable CONNECT-start callback. Keep its observed time,
				// without inventing a duration or confusing proxy TCP with server DNS.
				result := err
				if result == nil && response.StatusCode != http.StatusOK {
					result = errors.New("proxy connect rejected")
				}
				observer.done("proxy_connect", result)
			}
			return err
		}
		copyClient.Transport = clone
	}
	c.observation.mu.Lock()
	strictProbe := c.observation.strictProbe
	c.observation.mu.Unlock()
	if strictProbe {
		target, _ := url.Parse(c.creds.ServerURL)
		copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if target == nil || req.URL.Scheme != target.Scheme || req.URL.Host != target.Host {
				return errors.New("connection redirect refused")
			}
			if len(via) >= 10 {
				return errors.New("connection redirect limit")
			}
			if client.CheckRedirect != nil {
				return client.CheckRedirect(req, via)
			}
			return nil
		}
	}
	return &copyClient, observer
}

func (o *transportObservation) context(ctx context.Context) context.Context {
	trace := &httptrace.ClientTrace{
		DNSStart:     func(httptrace.DNSStartInfo) { o.start("dns") },
		DNSDone:      func(info httptrace.DNSDoneInfo) { o.done("dns", info.Err) },
		ConnectStart: func(string, string) { o.start("connect") },
		ConnectDone:  func(_, _ string, err error) { o.done("connect", err) },
		TLSHandshakeStart: func() {
			o.mu.Lock()
			allowed := o.serverTLS
			// TLS to an HTTPS proxy is not target TLS. A failure there belongs
			// to the aggregate dial; keep target TLS N/A or not_observed.
			o.phase = "dial"
			if allowed {
				o.phase = "tls"
			}
			o.mu.Unlock()
			if allowed {
				o.start("tls")
			}
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			o.mu.Lock()
			allowed := o.serverTLS
			o.mu.Unlock()
			if allowed {
				o.done("tls", err)
			}
		},
		WroteRequest: func(httptrace.WroteRequestInfo) { o.start("ws_upgrade") },
	}
	return httptrace.WithClientTrace(ctx, trace)
}

func (c *Conn) observationPhaseState(stage, state string) {
	c.initConnectionObservation()
	o := &c.observation
	o.mu.Lock()
	defer o.mu.Unlock()
	p := o.phases[stage]
	p.state = state
	o.phases[stage] = p
}

func (c *Conn) observedControlClient() (*http.Client, func() net.Addr, *transportObservation) {
	client, from := localAddrClient()
	observed, trace := c.observeHTTPClient(client, true)
	return observed, from, trace
}
