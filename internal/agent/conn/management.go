package conn

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/netinfo"
	"gpuworks.ai/gpuworks/internal/proto"
)

// setLocal records the local end of a new connection to the server.
func (c *Conn) setLocal(addr net.Addr, viaProxy bool) {
	var ip net.IP
	if tcp, ok := addr.(*net.TCPAddr); ok {
		ip = tcp.IP
	}
	c.localMu.Lock()
	c.local, c.viaProxy = ip, viaProxy
	c.localMu.Unlock()
}

// management is this node's address on the management network.
func (c *Conn) management(inv proto.NetInventory) *proto.ManagementAddress {
	c.localMu.Lock()
	local, viaProxy := c.local, c.viaProxy
	c.localMu.Unlock()
	host := ""
	if u, err := url.Parse(c.creds.ServerURL); err == nil {
		host = u.Hostname()
	}
	return netinfo.Management(local, viaProxy, inv, netinfo.DefaultRouteIP(host))
}

// localAddrClient is the default HTTP client (proxy settings included) with a
// dialer that remembers the local address of the connection it made: after
// the HTTP upgrade the WebSocket no longer exposes its socket.
func localAddrClient() (*http.Client, func() net.Addr) {
	var mu sync.Mutex
	var local net.Addr
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, addr)
		if err == nil {
			mu.Lock()
			local = conn.LocalAddr()
			mu.Unlock()
		}
		return conn, err
	}
	return &http.Client{Transport: transport}, func() net.Addr {
		mu.Lock()
		defer mu.Unlock()
		return local
	}
}

// proxiedServer reports whether the WebSocket to serverURL goes through an
// HTTP proxy from the environment, as the default client's transport does.
func proxiedServer(serverURL string) bool {
	u, err := url.Parse(serverURL)
	if err != nil {
		return false
	}
	proxy, err := http.ProxyFromEnvironment(&http.Request{URL: u})
	return err == nil && proxy != nil
}
