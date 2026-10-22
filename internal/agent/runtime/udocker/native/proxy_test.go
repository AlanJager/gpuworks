package native

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// connectProxy is a minimal HTTP CONNECT forward proxy that records the
// tunnels it opened.
type connectProxy struct {
	server *httptest.Server
	mu     sync.Mutex
	hosts  []string
}

func newConnectProxy(t *testing.T) *connectProxy {
	t.Helper()
	p := &connectProxy{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		p.mu.Lock()
		p.hosts = append(p.hosts, r.Host)
		p.mu.Unlock()
		// Loopback is never proxied, so a test names its registry
		// example.com (which the test server's certificate covers); the
		// proxy knows where that is.
		target := r.Host
		if host, port, err := net.SplitHostPort(target); err == nil && host == "example.com" {
			target = net.JoinHostPort("127.0.0.1", port)
		}
		upstream, err := net.Dial("tcp", target)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		go func() { _, _ = io.Copy(upstream, client); upstream.Close() }()
		go func() { _, _ = io.Copy(client, upstream); client.Close() }()
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *connectProxy) tunnels() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.hosts...)
}

// proxiedEngine is engineFor with the production proxy selector installed on
// the transport that trusts the fake registry's certificate.
func proxiedEngine(t *testing.T, reg *fakeRegistry) *Engine {
	t.Helper()
	engine, _ := engineFor(t, reg, 0)
	transport := reg.server.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = RequestProxy
	engine.Client.HTTP = &http.Client{Transport: transport}
	return engine
}

func TestNativePullUsesDeploymentProxy(t *testing.T) {
	reg := newFakeRegistry(t, "team/app", [][]byte{buildLayer(t, true, []entry{{name: "a.txt", body: "a"}})}, []byte(`{"config":{}}`))
	proxy := newConnectProxy(t)
	engine := proxiedEngine(t, reg)

	// The proxy is given as a bare host:port, the form operators paste.
	ctx := runtime.WithPullProxy(context.Background(), strings.TrimPrefix(proxy.server.URL, "http://"))
	registry := strings.Replace(reg.host(), "127.0.0.1", "example.com", 1)
	image := registry + "/" + reg.repo + ":latest"
	if err := engine.Pull(ctx, image, nil); err != nil {
		t.Fatalf("pull through proxy: %v", err)
	}
	tunnels := proxy.tunnels()
	if len(tunnels) == 0 {
		t.Fatal("native pull bypassed the deployment proxy")
	}
	for _, host := range tunnels {
		if host != registry {
			t.Fatalf("proxy tunnelled to %q, want only the registry %q", host, registry)
		}
	}
	if !engine.HasImage(image) {
		t.Fatal("proxied pull did not register the image")
	}
}

func TestNativePullWithoutDeploymentProxyGoesDirect(t *testing.T) {
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if os.Getenv(name) != "" {
			t.Skipf("%s is set in the test environment; direct routing cannot be asserted", name)
		}
	}
	reg := newFakeRegistry(t, "team/app", [][]byte{buildLayer(t, true, []entry{{name: "a.txt", body: "a"}})}, []byte(`{"config":{}}`))
	proxy := newConnectProxy(t)
	engine := proxiedEngine(t, reg)
	if err := engine.Pull(context.Background(), reg.image(), nil); err != nil {
		t.Fatal(err)
	}
	if tunnels := proxy.tunnels(); len(tunnels) != 0 {
		t.Fatalf("pull without a deployment proxy was proxied: %v", tunnels)
	}
}

func TestRequestProxyRejectsMalformedProxy(t *testing.T) {
	req, _ := http.NewRequestWithContext(runtime.WithPullProxy(context.Background(), "http://"), http.MethodGet, "https://registry.example/v2/", nil)
	if _, err := RequestProxy(req); err == nil || !strings.Contains(err.Error(), "invalid image pull proxy") {
		t.Fatalf("malformed proxy accepted: %v", err)
	}
	req, _ = http.NewRequestWithContext(runtime.WithPullProxy(context.Background(), "socks5://10.0.0.1:1080"), http.MethodGet, "https://registry.example/v2/", nil)
	got, err := RequestProxy(req)
	if err != nil || got.String() != "socks5://10.0.0.1:1080" {
		t.Fatalf("explicit scheme not preserved: %v %v", got, err)
	}
}

func TestNewClientInstallsDeploymentProxy(t *testing.T) {
	transport := NewClient().HTTP.Transport.(*http.Transport)
	req, _ := http.NewRequestWithContext(runtime.WithPullProxy(context.Background(), "10.1.2.3:3128"), http.MethodGet, "https://registry.example/v2/", nil)
	got, err := transport.Proxy(req)
	if err != nil || got == nil || got.String() != "http://10.1.2.3:3128" {
		t.Fatalf("production transport ignores the deployment proxy: %v %v", got, err)
	}
}

func TestNativePullExplicitDirectBypassesAmbientProxy(t *testing.T) {
	// ProxyFromEnvironment caches its environment, so isolate this case from
	// other tests and prove the control request actually reaches the proxy.
	if os.Getenv("GPUWORKS_TEST_NATIVE_PROXY_CHILD") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestNativePullExplicitDirectBypassesAmbientProxy$", "-test.count=1")
		command.Env = append(os.Environ(), "GPUWORKS_TEST_NATIVE_PROXY_CHILD=1")
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("isolated proxy test: %v\n%s", err, out)
		}
		return
	}
	var proxyRequests atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyRequests.Add(1)
		http.Error(w, "ambient proxy unavailable", http.StatusBadGateway)
	}))
	defer proxy.Close()
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(name, proxy.URL)
	}
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
	reg := newFakeRegistry(t, "team/app", [][]byte{buildLayer(t, true, []entry{{name: "a.txt", body: "a"}})}, []byte(`{"config":{}}`))
	engine := proxiedEngine(t, reg)
	transport := engine.Client.HTTP.Transport.(*http.Transport)
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if strings.HasPrefix(address, "registry.proxy-test.invalid:") {
			address = reg.host()
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.ServerName = "127.0.0.1"
	_, port, _ := net.SplitHostPort(reg.host())
	image := strings.Replace(reg.image(), reg.host(), net.JoinHostPort("registry.proxy-test.invalid", port), 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := engine.Pull(ctx, image, nil); err == nil {
		t.Fatal("inherited proxy control unexpectedly succeeded")
	}
	before := proxyRequests.Load()
	if before == 0 {
		t.Fatal("control did not reach the ambient proxy")
	}
	if err := engine.Pull(runtime.WithPullProxy(ctx, ""), image, nil); err != nil {
		t.Fatalf("explicit direct pull failed: %v", err)
	}
	if proxyRequests.Load() != before {
		t.Fatal("explicit direct pull used the ambient proxy")
	}
	if !engine.HasImage(image) {
		t.Fatal("direct pull did not register the image")
	}
}
