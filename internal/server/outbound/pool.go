package outbound

import (
	"context"
	"net"
	"net/http"
	"net/url"
)

// Proxy changes select a different pool; the destination stays in the request
// context, so dialing never resolves it a second time. Bound cached proxy
// configurations rather than retaining every configuration the administrator
// has ever tried. In-flight requests keep their transport until they finish.
const maxTransportPools = 8

func (t *SafeTransport) requestTransport(proxy *url.URL) *http.Transport {
	key := ""
	if proxy != nil {
		key = proxy.String()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if existing := t.transports[key]; existing != nil {
		return existing
	}
	transport := t.Transport.Clone()
	transport.Proxy = nil
	transport.DialTLSContext = nil
	transport.DialTLS = nil
	transport.DialContext = pinnedDial
	if proxy != nil {
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			target, err := checkedPinnedTarget(ctx, address)
			if err != nil {
				return nil, err
			}
			// net/http detaches dialing from request cancellation so it can
			// reuse a completed dial. A proxy handshake still belongs to the
			// initiating request and must not wait after it is canceled.
			ctx, cancel := context.WithCancel(ctx)
			stop := context.AfterFunc(target.requestContext, cancel)
			defer stop()
			defer cancel()
			var last error
			for _, ip := range target.addresses {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				destination := net.JoinHostPort(ip.Unmap().String(), target.port)
				var conn net.Conn
				if proxy.Scheme == "socks5" || proxy.Scheme == "socks5h" {
					conn, err = dialSOCKSTunnel(ctx, proxy, destination)
				} else {
					conn, err = dialProxyTunnel(ctx, proxy, destination, transport.TLSClientConfig)
				}
				if err == nil {
					return conn, nil
				}
				last = err
			}
			return nil, requestFailure{last}
		}
	}
	if t.transports == nil {
		t.transports = make(map[string]*http.Transport)
	}
	if len(t.transportOrder) == maxTransportPools {
		oldest := t.transportOrder[0]
		t.transports[oldest].CloseIdleConnections()
		delete(t.transports, oldest)
		t.transportOrder = t.transportOrder[1:]
	}
	t.transports[key] = transport
	t.transportOrder = append(t.transportOrder, key)
	return transport
}

func (t *SafeTransport) closeRetiredIdle(transport *http.Transport) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, active := range t.transports {
		if active == transport {
			return
		}
	}
	transport.CloseIdleConnections()
}

// CloseIdleConnections also covers cached transports and is safe while
// requests are in flight, matching the standard http.Transport contract.
func (t *SafeTransport) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Transport.CloseIdleConnections()
	for _, transport := range t.transports {
		transport.CloseIdleConnections()
	}
}
