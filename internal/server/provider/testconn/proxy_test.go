package testconn

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

const proxyPassword = "proxy-pass-9f3"

func proxyRunner(env map[string]string) *Runner {
	r := offlineRunner()
	r.Getenv = func(key string) string { return env[key] }
	r.Proxy = func(*http.Request) (*url.URL, error) {
		return url.Parse("http://ops:" + proxyPassword + "@proxy.internal:3128")
	}
	return r
}

// Through a proxy the server must reach the proxy, not the endpoint: DNS and
// TCP target it, the TLS handshake is the proxy's to make, and the vendor
// checks behind tcp_connect still run.
func TestProxyPathTargetsTheProxy(t *testing.T) {
	var proxied []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// A forward proxy receives the absolute URL of the endpoint.
		proxied = append(proxied, req.Method+" "+req.URL.String())
		w.Header().Set("Date", testNow.Format(http.TimeFormat))
	}))
	t.Cleanup(proxy.Close)
	r := proxyRunner(map[string]string{
		"no_proxy":    "localhost",
		"HTTPS_PROXY": "http://ops:" + proxyPassword + "@proxy.internal:3128",
		"HTTP_PROXY":  "http://ops:" + proxyPassword + "@proxy.internal:3128",
	})
	var resolved, asked []string
	r.LookupHost = func(_ context.Context, host string) ([]string, error) {
		resolved = append(resolved, host)
		return []string{"192.0.2.77"}, nil
	}
	r.Dial = dialTo(proxy, &asked)
	vendorRan := false
	res := run(context.Background(), r, Input{Kind: "k", Endpoint: "http://cloud.example:8080/zstack", Vendor: []VendorCheck{{
		Name: CheckAuthRequest, Depends: []string{CheckTCPConnect},
		Run: func(context.Context) (map[string]any, error) { vendorRan = true; return nil, nil },
	}}})
	want := []string{
		`{"seq":1,"name":"endpoint_format","status":"ok","duration_ms":0,"detail":{"host":"cloud.example:8080","scheme":"http"}}`,
		`{"seq":2,"name":"credentials_shape","status":"ok","duration_ms":0,"detail":{"fields":0}}`,
		`{"seq":3,"name":"dns_resolve","status":"ok","duration_ms":0,"detail":{"target":"proxy"}}`,
		`{"seq":4,"name":"tcp_connect","status":"ok","duration_ms":0,"detail":{"rtt_ms":0,"target":"proxy"}}`,
		`{"seq":5,"name":"tls_handshake","status":"skipped","duration_ms":0,"hint":"via_proxy"}`,
		`{"seq":6,"name":"proxy_detect","status":"ok","duration_ms":0,"detail":{"proxy":"env","used":true,"variables":["HTTPS_PROXY","HTTP_PROXY","no_proxy"]}}`,
		`{"seq":7,"name":"clock_skew","status":"ok","duration_ms":0,"detail":{"skew_seconds":0}}`,
		`{"seq":8,"name":"auth_request","status":"ok","duration_ms":0}`,
	}
	if len(res.checks) != len(want) {
		t.Fatalf("checks = %v", res.names())
	}
	for i, c := range res.checks {
		wantWire(t, c, want[i])
	}
	if !vendorRan || !res.summary.OK {
		t.Fatalf("vendor ran = %v, summary = %+v", vendorRan, res.summary)
	}
	if !reflect.DeepEqual(resolved, []string{"proxy.internal"}) {
		t.Fatalf("resolved %v, want the proxy's host only", resolved)
	}
	if !reflect.DeepEqual(asked, []string{"proxy.internal:3128", "proxy.internal:3128"}) {
		t.Fatalf("dialed %v, want the proxy for tcp_connect and clock_skew", asked)
	}
	if !reflect.DeepEqual(proxied, []string{"GET http://cloud.example:8080/zstack"}) {
		t.Fatalf("proxy saw %v", proxied)
	}
	assertNoProxyValueOnWire(t, res)
}

func assertNoProxyValueOnWire(t *testing.T, res runResult) {
	t.Helper()
	b, err := json.Marshal(res.checks)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{proxyPassword, "proxy.internal", "3128", "192.0.2.77"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("proxy variable value %q reached the wire: %s", leak, b)
		}
	}
}

func TestProxyFailuresUseProxyCode(t *testing.T) {
	const tcpSkipped = `{"seq":4,"name":"tcp_connect","status":"skipped","duration_ms":0,"detail":{"depends":"dns_resolve"},"hint":"depends_failed"}`
	t.Run("proxy host does not resolve", func(t *testing.T) {
		captureLog(t)
		r := proxyRunner(nil)
		r.LookupHost = func(context.Context, string) ([]string, error) { return nil, errors.New("no such host") }
		res := run(context.Background(), r, Input{Kind: "k", Endpoint: "https://cloud.example"})
		wantWire(t, res.check(t, CheckDNSResolve), `{"seq":3,"name":"dns_resolve","status":"failed","duration_ms":0,"code":"PROVIDER_PROXY_FAILED","detail":{"target":"proxy"},"hint":"proxy"}`)
		wantWire(t, res.check(t, CheckTCPConnect), tcpSkipped)
		// No variable is set, yet the endpoint is proxied: still reported.
		wantWire(t, res.check(t, CheckProxyDetect), `{"seq":6,"name":"proxy_detect","status":"ok","duration_ms":0,"detail":{"proxy":"env","used":true,"variables":[]}}`)
		assertNoProxyValueOnWire(t, res)
	})
	t.Run("proxy refuses the connection", func(t *testing.T) {
		captureLog(t)
		r := proxyRunner(nil)
		vendorRan := false
		res := run(context.Background(), r, Input{Kind: "k", Endpoint: "https://cloud.example", Vendor: []VendorCheck{{
			Name: CheckAuthRequest, Depends: []string{CheckTCPConnect},
			Run: func(context.Context) (map[string]any, error) { vendorRan = true; return nil, nil },
		}}})
		wantWire(t, res.check(t, CheckTCPConnect), `{"seq":4,"name":"tcp_connect","status":"failed","duration_ms":0,"code":"PROVIDER_PROXY_FAILED","detail":{"target":"proxy"},"hint":"proxy"}`)
		wantWire(t, res.check(t, CheckTLSHandshake), `{"seq":5,"name":"tls_handshake","status":"skipped","duration_ms":0,"detail":{"depends":"tcp_connect"},"hint":"depends_failed"}`)
		if vendorRan {
			t.Fatal("vendor check ran behind an unreachable proxy")
		}
		if !reflect.DeepEqual(res.summary.Failed, []string{CheckTCPConnect}) {
			t.Fatalf("summary = %+v", res.summary)
		}
		assertNoProxyValueOnWire(t, res)
	})
	t.Run("proxy cannot open the tunnel", func(t *testing.T) {
		captureLog(t)
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "denied", http.StatusForbidden)
		}))
		t.Cleanup(proxy.Close)
		r := proxyRunner(nil)
		r.Dial = dialTo(proxy, nil)
		res := run(context.Background(), r, Input{Kind: "k", Endpoint: "https://cloud.example"})
		wantWire(t, res.check(t, CheckClockSkew), `{"seq":7,"name":"clock_skew","status":"failed","duration_ms":0,"code":"PROVIDER_PROXY_FAILED","detail":{"target":"proxy"},"hint":"proxy"}`)
		assertNoProxyValueOnWire(t, res)
	})
}

// A proxy variable that is set but not used for this endpoint (NO_PROXY
// matches it) leaves the checks on the endpoint itself.
func TestProxySetButNotUsed(t *testing.T) {
	captureLog(t)
	r := offlineRunner()
	r.Getenv = func(key string) string {
		return map[string]string{"https_proxy": "http://proxy.internal:3128", "NO_PROXY": "cloud.example"}[key]
	}
	var asked []string
	r.Dial = func(_ context.Context, _, address string) (net.Conn, error) {
		asked = append(asked, address)
		return nil, errors.New("down")
	}
	res := run(context.Background(), r, Input{Kind: "k", Endpoint: "https://cloud.example"})
	wantWire(t, res.check(t, CheckProxyDetect), `{"seq":6,"name":"proxy_detect","status":"ok","duration_ms":0,"detail":{"proxy":"env","used":false,"variables":["NO_PROXY","https_proxy"]}}`)
	if c := res.check(t, CheckTCPConnect); c.Code != messages.ErrorProviderTCPUnreachable {
		t.Fatalf("tcp_connect = %+v, want a direct failure code", c)
	}
	if !reflect.DeepEqual(asked, []string{"cloud.example:443"}) {
		t.Fatalf("dialed %v", asked)
	}
}

// An unusable proxy setting fails proxy_detect. Its error quotes the
// variable's value, so neither the wire nor the log may carry it.
func TestInvalidProxySettingIsReportedWithoutItsValue(t *testing.T) {
	logs := captureLog(t)
	r := offlineRunner()
	r.Proxy = func(*http.Request) (*url.URL, error) {
		return nil, errors.New(`invalid proxy address "http://ops:` + proxyPassword + `@proxy.internal:port"`)
	}
	res := run(context.Background(), r, Input{Kind: "k", Endpoint: "http://cloud.example"})
	wantWire(t, res.check(t, CheckProxyDetect), `{"seq":6,"name":"proxy_detect","status":"failed","duration_ms":0,"code":"PROVIDER_PROXY_FAILED","hint":"proxy"}`)
	assertNoProxyValueOnWire(t, res)
	if strings.Contains(logs.String(), proxyPassword) || strings.Contains(logs.String(), "proxy.internal") {
		t.Fatalf("proxy variable value reached the log: %s", logs)
	}
	if !strings.Contains(logs.String(), "check=proxy_detect") {
		t.Fatalf("proxy_detect failure was not logged: %s", logs)
	}
}
