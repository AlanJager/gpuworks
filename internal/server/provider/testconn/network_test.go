package testconn

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestTCPFailureClassified(t *testing.T) {
	cases := map[string]struct {
		err        error
		code, hint string
	}{
		"refused":     {syscall.ECONNREFUSED, messages.ErrorProviderTCPRefused, "tcp_refused"},
		"unreachable": {syscall.EHOSTUNREACH, messages.ErrorProviderTCPUnreachable, "tcp_unreachable"},
		"timeout":     {os.ErrDeadlineExceeded, messages.ErrorProviderTCPTimeout, "tcp_timeout"},
		"other":       {errors.New("boom"), messages.ErrorProviderTCPUnreachable, "tcp_unreachable"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			captureLog(t)
			r := offlineRunner()
			var asked []string
			r.Dial = func(_ context.Context, _, address string) (net.Conn, error) {
				asked = append(asked, address)
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: tc.err}
			}
			res := run(context.Background(), r, Input{Kind: "k", Endpoint: "https://cloud.example/zstack"})
			wantWire(t, res.check(t, CheckDNSResolve), `{"seq":3,"name":"dns_resolve","status":"ok","duration_ms":0,"detail":{"ips":["192.0.2.10"]}}`)
			wantWire(t, res.check(t, CheckTCPConnect), `{"seq":4,"name":"tcp_connect","status":"failed","duration_ms":0,"code":"`+tc.code+`","hint":"`+tc.hint+`"}`)
			if !reflect.DeepEqual(asked, []string{"cloud.example:443"}) {
				t.Fatalf("dialed %v, want the https default port once", asked)
			}
			for _, name := range []string{CheckTLSHandshake, CheckClockSkew} {
				if c := res.check(t, name); c.Status != StatusSkipped || c.Hint != HintDependsFailed {
					t.Fatalf("%s = %+v, want skipped behind tcp_connect", name, c)
				}
			}
		})
	}
}

func TestDNSSkippedForLiteralIP(t *testing.T) {
	captureLog(t)
	r := offlineRunner()
	r.LookupHost = func(context.Context, string) ([]string, error) {
		t.Error("literal IP must not hit the resolver")
		return nil, errors.New("unexpected")
	}
	res := run(context.Background(), r, Input{Kind: "k", Endpoint: "http://192.0.2.1:8080/zstack"})
	wantWire(t, res.check(t, CheckDNSResolve), `{"seq":3,"name":"dns_resolve","status":"ok","duration_ms":0,"detail":{"ips":["192.0.2.1"],"literal":true}}`)
}

func TestPlainHTTPEndpointPassesEveryNetworkCheck(t *testing.T) {
	server := dateServer(t, testNow.Add(-10*time.Second).Format(http.TimeFormat))
	r := offlineRunner()
	var asked []string
	r.Dial = dialTo(server, &asked)
	res := run(context.Background(), r, Input{Kind: "k", Endpoint: "http://cloud.example:8080/zstack"})
	want := []string{
		`{"seq":1,"name":"endpoint_format","status":"ok","duration_ms":0,"detail":{"host":"cloud.example:8080","scheme":"http"}}`,
		`{"seq":2,"name":"credentials_shape","status":"ok","duration_ms":0,"detail":{"fields":0}}`,
		`{"seq":3,"name":"dns_resolve","status":"ok","duration_ms":0,"detail":{"ips":["192.0.2.10"]}}`,
		`{"seq":4,"name":"tcp_connect","status":"ok","duration_ms":0,"detail":{"address":"cloud.example:8080","rtt_ms":0}}`,
		`{"seq":5,"name":"tls_handshake","status":"ok","duration_ms":0,"detail":{"mode":"plain_http"}}`,
		`{"seq":6,"name":"proxy_detect","status":"ok","duration_ms":0,"detail":{"proxy":"none"}}`,
		`{"seq":7,"name":"clock_skew","status":"ok","duration_ms":0,"detail":{"skew_seconds":10}}`,
	}
	if len(res.checks) != len(want) {
		t.Fatalf("checks = %v", res.names())
	}
	for i, c := range res.checks {
		wantWire(t, c, want[i])
	}
	if res.err != nil || !res.summary.OK || res.summary.Failed != nil {
		t.Fatalf("summary = %+v, err %v", res.summary, res.err)
	}
	// tcp_connect and the clock-skew request both went through the injected
	// dialer, to the endpoint's own address.
	if !reflect.DeepEqual(asked, []string{"cloud.example:8080", "cloud.example:8080"}) {
		t.Fatalf("dialed %v", asked)
	}
}

func TestClockSkew(t *testing.T) {
	cases := map[string]struct {
		date string
		want string
	}{
		"in range behind": {testNow.Add(-4 * time.Minute).Format(http.TimeFormat),
			`{"seq":7,"name":"clock_skew","status":"ok","duration_ms":0,"detail":{"skew_seconds":240}}`},
		"in range ahead": {testNow.Add(90 * time.Second).Format(http.TimeFormat),
			`{"seq":7,"name":"clock_skew","status":"ok","duration_ms":0,"detail":{"skew_seconds":90}}`},
		"out of range": {testNow.Add(-10 * time.Minute).Format(http.TimeFormat),
			`{"seq":7,"name":"clock_skew","status":"failed","duration_ms":0,"code":"PROVIDER_CLOCK_SKEW","detail":{"skew_seconds":600},"hint":"clock_skew"}`},
		"out of range ahead": {testNow.Add(6 * time.Minute).Format(http.TimeFormat),
			`{"seq":7,"name":"clock_skew","status":"failed","duration_ms":0,"code":"PROVIDER_CLOCK_SKEW","detail":{"skew_seconds":360},"hint":"clock_skew"}`},
		"missing": {"",
			`{"seq":7,"name":"clock_skew","status":"ok","duration_ms":0,"detail":{"date_header":"absent"}}`},
		"unparseable": {"yesterday at noon",
			`{"seq":7,"name":"clock_skew","status":"ok","duration_ms":0,"detail":{"date_header":"unparseable"}}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			captureLog(t)
			r := offlineRunner()
			r.Dial = dialTo(dateServer(t, tc.date), nil)
			res := run(context.Background(), r, Input{Kind: "k", Endpoint: "http://cloud.example"})
			wantWire(t, res.check(t, CheckClockSkew), tc.want)
		})
	}
}

// An endpoint that accepts the connection and never answers HTTP fails
// clock_skew as unreachable over HTTP.
func TestClockSkewNoHTTPAnswer(t *testing.T) {
	captureLog(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	r := offlineRunner()
	r.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, listener.Addr().String())
	}
	res := run(context.Background(), r, Input{Kind: "k", Endpoint: "http://cloud.example"})
	wantWire(t, res.check(t, CheckClockSkew), `{"seq":7,"name":"clock_skew","status":"failed","duration_ms":0,"code":"PROVIDER_HTTP_UNREACHABLE","hint":"http"}`)
}

func tlsDateServer(t *testing.T) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Date", testNow.Format(http.TimeFormat))
	}))
	t.Cleanup(server.Close)
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	return server, pool
}

// The handshake runs over the injected dialer's connection and reads the
// certificate's remaining life from the runner's clock.
func TestTLSHandshakeThroughInjectedDial(t *testing.T) {
	server, pool := tlsDateServer(t)
	r := offlineRunner()
	var asked []string
	r.Dial = dialTo(server, &asked)
	r.TLSConfig = &tls.Config{RootCAs: pool}
	// The test server's certificate names example.com.
	res := run(context.Background(), r, Input{Kind: "k", Endpoint: "https://example.com:8443/api"})
	days := int(server.Certificate().NotAfter.Sub(testNow).Hours() / 24)
	wantWire(t, res.check(t, CheckTLSHandshake), fmt.Sprintf(`{"seq":5,"name":"tls_handshake","status":"ok","duration_ms":0,"detail":{"cert_days_left":%d,"server_name":"example.com"}}`, days))
	wantWire(t, res.check(t, CheckClockSkew), `{"seq":7,"name":"clock_skew","status":"ok","duration_ms":0,"detail":{"skew_seconds":0}}`)
	if !reflect.DeepEqual(asked, []string{"example.com:8443", "example.com:8443", "example.com:8443"}) {
		t.Fatalf("dialed %v, want tcp_connect, tls_handshake and clock_skew through the injected dialer", asked)
	}
	if !res.summary.OK {
		t.Fatalf("summary = %+v", res.summary)
	}
}

func TestTLSHandshakeFailures(t *testing.T) {
	server, pool := tlsDateServer(t)
	const failed = `{"seq":5,"name":"tls_handshake","status":"failed","duration_ms":0,"code":"PROVIDER_TLS_FAILED","hint":"tls"}`
	t.Run("untrusted certificate", func(t *testing.T) {
		logs := captureLog(t)
		r := offlineRunner()
		r.Dial = dialTo(server, nil)
		r.TLSConfig = &tls.Config{RootCAs: x509.NewCertPool()}
		res := run(context.Background(), r, Input{Kind: "k", Endpoint: "https://example.com"})
		wantWire(t, res.check(t, CheckTLSHandshake), failed)
		if logs.Len() == 0 {
			t.Fatal("the handshake's cause was not logged")
		}
	})
	t.Run("name mismatch", func(t *testing.T) {
		captureLog(t)
		r := offlineRunner()
		r.Dial = dialTo(server, nil)
		r.TLSConfig = &tls.Config{RootCAs: pool}
		res := run(context.Background(), r, Input{Kind: "k", Endpoint: "https://cloud.invalid"})
		wantWire(t, res.check(t, CheckTLSHandshake), failed)
	})
	t.Run("expired by the runner's clock", func(t *testing.T) {
		captureLog(t)
		r := offlineRunner()
		r.Now = func() time.Time { return server.Certificate().NotAfter.Add(24 * time.Hour) }
		r.Dial = dialTo(server, nil)
		r.TLSConfig = &tls.Config{RootCAs: pool}
		res := run(context.Background(), r, Input{Kind: "k", Endpoint: "https://example.com"})
		wantWire(t, res.check(t, CheckTLSHandshake), failed)
	})
	t.Run("plain server", func(t *testing.T) {
		captureLog(t)
		r := offlineRunner()
		r.Dial = dialTo(dateServer(t, ""), nil)
		res := run(context.Background(), r, Input{Kind: "k", Endpoint: "https://example.com"})
		wantWire(t, res.check(t, CheckTLSHandshake), failed)
	})
}
