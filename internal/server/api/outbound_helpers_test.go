package api

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// useManagementProxy makes proxyURL the management node's outbound proxy: the
// only proxy entry, chosen for the management node.
func useManagementProxy(t *testing.T, st *store.Store, proxyURL string) {
	t.Helper()
	existing, err := st.PullProxies()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range existing {
		if err := st.DeletePullProxy(entry.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CreatePullProxy(store.PullProxySpec{HTTPProxy: proxyURL, ControlPlane: true}, ""); err != nil {
		t.Fatal(err)
	}
}

// storeBrokenManagementProxy stores a proxy entry no client can use, as an
// older release or a hand edit could leave one: requests through it must
// fail rather than go direct.
func storeBrokenManagementProxy(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.DB().Exec(`UPDATE pull_proxies SET http_proxy = 'broken://invalid'`).Error; err != nil {
		t.Fatal(err)
	}
	// The entries are cached between writes; a write through the store
	// clears the cache the raw update did not.
	if err := st.ReorderPullProxies(proxyIDs(t, st)); err != nil {
		t.Fatal(err)
	}
}

func proxyIDs(t *testing.T, st *store.Store) []uint {
	t.Helper()
	proxies, err := st.PullProxies()
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]uint, 0, len(proxies))
	for _, proxy := range proxies {
		ids = append(ids, proxy.ID)
	}
	return ids
}

// viaProxy names a loopback test server by a host only the test proxy
// reaches: loopback is never proxied.
func viaProxy(raw string) string {
	return strings.Replace(raw, "127.0.0.1", "8.8.8.8", 1)
}

// newTunnelingProxy forwards only to a fixed local fixture. It models a real
// CONNECT proxy while asserting the server pins the requested origin IP.
func newTunnelingProxy(t *testing.T, origin string, onConnect func()) *httptest.Server {
	t.Helper()
	target, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != "8.8.8.8:"+target.Port() {
			t.Errorf("proxy target=%s %s", r.Method, r.Host)
			w.WriteHeader(400)
			return
		}
		if onConnect != nil {
			onConnect()
		}
		upstream, err := net.Dial("tcp", target.Host)
		if err != nil {
			t.Error(err)
			w.WriteHeader(502)
			return
		}
		defer upstream.Close()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, conn); upstream.Close(); close(done) }()
		_, _ = io.Copy(conn, upstream)
		conn.Close()
		<-done
	}))
}
