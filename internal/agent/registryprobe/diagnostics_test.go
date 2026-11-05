package registryprobe

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// go test ./internal/agent/registryprobe -run '^TestRegistryProbeDiagnostic' -count=1
func TestRegistryProbeDiagnosticKeepsOriginalCachedObservation(t *testing.T) {
	n, p := newFakeNet(map[string]behavior{"registry.example": {status: http.StatusUnauthorized}})
	firstAt := n.clock.Now().UTC().Format(time.RFC3339Nano)
	first := Report{Hosts: []HostResult{p.Probe(t.Context(), []string{"registry.example"}, DirectRoute)["registry.example"]}}.String()
	for _, want := range []string{"HTTP 401", "observed_at=" + firstAt, "GET /v2/", "from=node", "image authorization, tag, manifest and layers not checked"} {
		if !strings.Contains(first, want) {
			t.Errorf("diagnostic %q lacks %q", first, want)
		}
	}
	n.clock.Advance(10 * time.Minute)
	cached := Report{Hosts: []HostResult{p.Probe(context.Background(), []string{"registry.example"}, DirectRoute)["registry.example"]}}.String()
	if cached != first || n.callCount("registry.example") != 1 {
		t.Fatalf("cache read fabricated a new observation: first=%q cached=%q calls=%d", first, cached, n.callCount("registry.example"))
	}
	n.clock.Advance(20 * time.Minute)
	fresh := Report{Hosts: []HostResult{p.Probe(context.Background(), []string{"registry.example"}, DirectRoute)["registry.example"]}}.String()
	if fresh == cached || n.callCount("registry.example") != 2 || !strings.Contains(fresh, "observed_at="+n.clock.Now().UTC().Format(time.RFC3339Nano)) {
		t.Fatalf("expiry did not obtain fresh evidence: %q calls=%d", fresh, n.callCount("registry.example"))
	}
}

type diagnosticTransport func(*http.Request) (*http.Response, error)

func (f diagnosticTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRegistryProbeDiagnosticClassifiesFailuresWithoutRawSecrets(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"dns", &net.DNSError{Name: "private-internal-host", Err: "secret-dns-detail"}, "dns"},
		{"refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "connection_refused"},
		{"timeout", context.DeadlineExceeded, "timeout"},
		{"cancelled", context.Canceled, "cancelled"},
		{"tls", &tls.CertificateVerificationError{Err: errors.New("secret-certificate")}, "tls"},
		{"generic", errors.New("http://secret-user:secret-password@proxy.example?token=secret-query"), "transport_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, p := newFakeNet(map[string]behavior{"registry.example": {err: tc.err}})
			res := p.Probe(t.Context(), []string{"registry.example"}, DirectRoute)["registry.example"]
			if res.Reachable || res.Failure != tc.want || res.ObservedAt.IsZero() || res.Path != "unknown" {
				t.Fatalf("result = %+v", res)
			}
			report := Report{Hosts: []HostResult{res}}.String()
			if !strings.Contains(report, "failure="+tc.want) || strings.Contains(report, "secret-") || strings.Contains(report, "private-internal-host") {
				t.Fatalf("unsafe or missing classification: %q", report)
			}
		})
	}
}

func TestRegistryProbeDiagnosticResolvesActualProxyExactlyOnce(t *testing.T) {
	for _, bypass := range []bool{false, true} {
		t.Run(map[bool]string{false: "proxy", true: "no_proxy"}[bypass], func(t *testing.T) {
			configured := RouteFor("", "http://secret-user:secret-password@proxy.example:3128", "")
			if bypass {
				configured = RouteFor("", "http://secret-user:secret-password@proxy.example:3128", "registry.example")
			}
			resolve := configured.Proxy
			var calls atomic.Int32
			configured.Proxy = func(req *http.Request) (*url.URL, error) {
				calls.Add(1)
				return resolve(req)
			}
			p := New(WithTransport(func(route Route) http.RoundTripper {
				return diagnosticTransport(func(req *http.Request) (*http.Response, error) {
					// Like net/http.Transport, the fixture resolves Proxy once.
					if _, err := route.Proxy(req); err != nil {
						return nil, err
					}
					return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("secret-body")), Header: http.Header{}}, nil
				})
			}))
			res := p.Probe(t.Context(), []string{"registry.example"}, configured)["registry.example"]
			want := "proxy"
			if bypass {
				want = "direct"
			}
			if calls.Load() != 1 || res.Path != want || !res.Reachable || res.HTTPStatus != 401 {
				t.Fatalf("proxy calls=%d result=%+v", calls.Load(), res)
			}
			report := Report{Hosts: []HostResult{res}}.String()
			if strings.Contains(report, "secret-") || strings.Contains(report, "proxy.example") || strings.Contains(report, configured.Key) {
				t.Fatalf("proxy credentials/key or body leaked: %s", report)
			}
		})
	}
}

func TestRegistryProbeDiagnosticObservesDefaultTransportResolution(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/" {
			t.Errorf("unexpected probe path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	var calls atomic.Int32
	route := Route{Key: "test-secret-cache-key", Proxy: func(*http.Request) (*url.URL, error) {
		calls.Add(1)
		return nil, nil
	}}
	p := New(WithTransport(func(route Route) http.RoundTripper {
		tr := defaultTransport(route).(*http.Transport)
		tr.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		return tr
	}))
	host := strings.TrimPrefix(server.URL, "https://")
	res := p.Probe(t.Context(), []string{host}, route)[host]
	if calls.Load() != 1 || res.Path != "direct" || !res.Reachable || res.HTTPStatus != 401 || res.Failure != "" {
		t.Fatalf("actual default transport: calls=%d result=%+v", calls.Load(), res)
	}
	p.Probe(t.Context(), []string{host}, route)
	if calls.Load() != 1 {
		t.Fatalf("cache read repeated proxy resolution: %d calls", calls.Load())
	}
}
