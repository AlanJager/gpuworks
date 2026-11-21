package testconn

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestEndpointFormatRejectsBadInput(t *testing.T) {
	cases := map[string]struct{ endpoint, hint string }{
		"empty":    {"  ", "endpoint_empty"},
		"scheme":   {"ftp://x", "endpoint_shape"},
		"no host":  {"http://:8080", "endpoint_shape"},
		"userinfo": {"https://user:pass@cloud.example", "endpoint_shape"},
		"query":    {"https://cloud.example/?a=b", "endpoint_shape"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := offlineRunner()
			r.LookupHost = func(context.Context, string) ([]string, error) {
				t.Error("an invalid endpoint must not reach the resolver")
				return nil, errors.New("unexpected")
			}
			vendorRan := false
			res := run(context.Background(), r, Input{Kind: "k", Endpoint: tc.endpoint, Vendor: []VendorCheck{{
				Name: CheckAuthRequest, Depends: []string{CheckTCPConnect},
				Run: func(context.Context) (map[string]any, error) { vendorRan = true; return nil, nil },
			}}})
			if res.err != nil {
				t.Fatal(res.err)
			}
			wantWire(t, res.checks[0], `{"seq":1,"name":"endpoint_format","status":"failed","duration_ms":0,"code":"PROVIDER_ENDPOINT_INVALID","hint":"`+tc.hint+`"}`)
			// credentials_shape does not need the endpoint and still runs;
			// everything that needs one is skipped and names the cause.
			wantWire(t, res.checks[1], `{"seq":2,"name":"credentials_shape","status":"ok","duration_ms":0,"detail":{"fields":0}}`)
			want := []string{CheckEndpointFormat, CheckCredentialsShape, CheckDNSResolve, CheckTCPConnect, CheckTLSHandshake, CheckProxyDetect, CheckClockSkew, CheckAuthRequest}
			if !reflect.DeepEqual(res.names(), want) {
				t.Fatalf("checks = %v, want %v", res.names(), want)
			}
			for i, c := range res.checks[2:] {
				wantWire(t, c, `{"seq":`+string(rune('3'+i))+`,"name":"`+c.Name+`","status":"skipped","duration_ms":0,"detail":{"depends":"endpoint_format"},"hint":"depends_failed"}`)
			}
			if vendorRan {
				t.Fatal("vendor check ran without a valid endpoint")
			}
			if res.summary.OK || !reflect.DeepEqual(res.summary.Failed, []string{CheckEndpointFormat}) {
				t.Fatalf("summary = %+v", res.summary)
			}
		})
	}
}

func TestEndpointFormatDetailIsSchemeAndHost(t *testing.T) {
	res := run(context.Background(), offlineRunner(), Input{Kind: "k", Endpoint: " http://192.0.2.1:8080/zstack "})
	wantWire(t, res.check(t, CheckEndpointFormat), `{"seq":1,"name":"endpoint_format","status":"ok","duration_ms":0,"detail":{"host":"192.0.2.1:8080","scheme":"http"}}`)
}

func TestCredentialsShapeNamesMissingField(t *testing.T) {
	in := Input{
		Kind:                 "zstack",
		Endpoint:             "://bad",
		RequiredCredentials:  []string{"public_key", "private_key"},
		CredentialsSatisfied: func(name string) bool { return name == "public_key" },
	}
	res := run(context.Background(), offlineRunner(), in)
	wantWire(t, res.check(t, CheckCredentialsShape), `{"seq":2,"name":"credentials_shape","status":"failed","duration_ms":0,"code":"PROVIDER_CREDENTIAL_MISSING","detail":{"field":"private_key"},"hint":"credential_missing"}`)

	in.CredentialsSatisfied = func(string) bool { return true }
	res = run(context.Background(), offlineRunner(), in)
	wantWire(t, res.check(t, CheckCredentialsShape), `{"seq":2,"name":"credentials_shape","status":"ok","duration_ms":0,"detail":{"fields":2}}`)
}

// A kind that takes no endpoint skips the endpoint and network checks as not
// applicable, and that skip blocks nothing: a vendor check that depends on
// tcp_connect still runs.
func TestEndpointNotApplicableSkipsNetworkAndRunsVendor(t *testing.T) {
	r := offlineRunner()
	r.Dial = func(context.Context, string, string) (net.Conn, error) {
		t.Error("no endpoint: nothing to dial")
		return nil, errors.New("unexpected")
	}
	in := Input{
		Kind: "k", EndpointNotApplicable: true,
		RequiredCredentials:  []string{"public_key"},
		CredentialsSatisfied: func(string) bool { return true },
		Vendor: []VendorCheck{
			{Name: CheckAuthRequest, Depends: []string{CheckCredentialsShape, CheckTCPConnect},
				Run: func(context.Context) (map[string]any, error) { return nil, nil }},
		},
	}
	res := run(context.Background(), r, in)
	want := []string{
		`{"seq":1,"name":"endpoint_format","status":"skipped","duration_ms":0,"hint":"not_applicable"}`,
		`{"seq":2,"name":"credentials_shape","status":"ok","duration_ms":0,"detail":{"fields":1}}`,
		`{"seq":3,"name":"dns_resolve","status":"skipped","duration_ms":0,"hint":"not_applicable"}`,
		`{"seq":4,"name":"tcp_connect","status":"skipped","duration_ms":0,"hint":"not_applicable"}`,
		`{"seq":5,"name":"tls_handshake","status":"skipped","duration_ms":0,"hint":"not_applicable"}`,
		`{"seq":6,"name":"proxy_detect","status":"skipped","duration_ms":0,"hint":"not_applicable"}`,
		`{"seq":7,"name":"clock_skew","status":"skipped","duration_ms":0,"hint":"not_applicable"}`,
		`{"seq":8,"name":"auth_request","status":"ok","duration_ms":0}`,
	}
	if len(res.checks) != len(want) {
		t.Fatalf("checks = %v", res.names())
	}
	for i, c := range res.checks {
		wantWire(t, c, want[i])
	}
	if !res.summary.OK || res.summary.Failed != nil {
		t.Fatalf("summary = %+v", res.summary)
	}

	// A missing credential still blocks the vendor check.
	in.CredentialsSatisfied = func(string) bool { return false }
	res = run(context.Background(), r, in)
	wantWire(t, res.check(t, CheckAuthRequest), `{"seq":8,"name":"auth_request","status":"skipped","duration_ms":0,"detail":{"depends":"credentials_shape"},"hint":"depends_failed"}`)
}

// A failed check blocks its dependents transitively; an independent check
// still runs. Failed lists names in report order, not map order.
func TestDependenciesAndFailedOrder(t *testing.T) {
	r := offlineRunner()
	r.LookupHost = func(context.Context, string) ([]string, error) { return nil, errors.New("no such host") }
	ran := map[string]bool{}
	vendor := func(name string, depends []string, err error) VendorCheck {
		return VendorCheck{Name: name, Depends: depends, Run: func(context.Context) (map[string]any, error) {
			ran[name] = true
			return nil, err
		}}
	}
	in := Input{
		Kind: "k", Endpoint: "http://cloud.example:8080",
		RequiredCredentials:  []string{"api_key"},
		CredentialsSatisfied: func(string) bool { return false },
		Vendor: []VendorCheck{
			vendor(CheckAuthRequest, []string{CheckTCPConnect}, nil),
			vendor(CheckProtocolShape, []string{CheckAuthRequest}, nil),
			vendor(CheckAPIEndpoints, nil, errors.New("plain error")),
			vendor(CheckResourceSummary, []string{"never_reported"}, nil),
		},
	}
	for range 20 {
		res := run(context.Background(), r, in)
		want := []string{CheckEndpointFormat, CheckCredentialsShape, CheckDNSResolve, CheckTCPConnect, CheckTLSHandshake,
			CheckProxyDetect, CheckClockSkew, CheckAuthRequest, CheckProtocolShape, CheckAPIEndpoints, CheckResourceSummary}
		if !reflect.DeepEqual(res.names(), want) {
			t.Fatalf("checks = %v, want %v", res.names(), want)
		}
		for i, c := range res.checks {
			if c.Seq != i+1 {
				t.Fatalf("check %s seq = %d, want %d", c.Name, c.Seq, i+1)
			}
		}
		wantWire(t, res.check(t, CheckDNSResolve), `{"seq":3,"name":"dns_resolve","status":"failed","duration_ms":0,"code":"PROVIDER_DNS_FAILED","detail":{"host":"cloud.example"},"hint":"dns"}`)
		wantWire(t, res.check(t, CheckTCPConnect), `{"seq":4,"name":"tcp_connect","status":"skipped","duration_ms":0,"detail":{"depends":"dns_resolve"},"hint":"depends_failed"}`)
		wantWire(t, res.check(t, CheckTLSHandshake), `{"seq":5,"name":"tls_handshake","status":"skipped","duration_ms":0,"detail":{"depends":"tcp_connect"},"hint":"depends_failed"}`)
		wantWire(t, res.check(t, CheckProxyDetect), `{"seq":6,"name":"proxy_detect","status":"ok","duration_ms":0,"detail":{"proxy":"none"}}`)
		wantWire(t, res.check(t, CheckProtocolShape), `{"seq":9,"name":"protocol_shape","status":"skipped","duration_ms":0,"detail":{"depends":"auth_request"},"hint":"depends_failed"}`)
		// An error that is not a Failure still fails with a product code.
		wantWire(t, res.check(t, CheckAPIEndpoints), `{"seq":10,"name":"api_endpoints","status":"failed","duration_ms":0,"code":"PROVIDER_CHECK_FAILED"}`)
		wantWire(t, res.check(t, CheckResourceSummary), `{"seq":11,"name":"resource_summary","status":"skipped","duration_ms":0,"detail":{"depends":"never_reported"},"hint":"depends_failed"}`)
		if !reflect.DeepEqual(res.summary, Summary{Failed: []string{CheckCredentialsShape, CheckDNSResolve, CheckAPIEndpoints}}) {
			t.Fatalf("summary = %+v", res.summary)
		}
	}
	if ran[CheckAuthRequest] || ran[CheckProtocolShape] || ran[CheckResourceSummary] || !ran[CheckAPIEndpoints] {
		t.Fatalf("vendor checks ran = %v", ran)
	}
}

// Durations come from the runner's clock, not the wall clock.
func TestDurationsUseInjectedClock(t *testing.T) {
	server := dateServer(t, testNow.Format(time.RFC1123))
	r := offlineRunner()
	now := testNow
	r.Now = func() time.Time {
		now = now.Add(7 * time.Millisecond)
		return now
	}
	r.Dial = dialTo(server, nil)
	res := run(context.Background(), r, Input{Kind: "k", Endpoint: "http://cloud.example"})
	wantWire(t, res.check(t, CheckEndpointFormat), `{"seq":1,"name":"endpoint_format","status":"ok","duration_ms":7,"detail":{"host":"cloud.example","scheme":"http"}}`)
	// The check reads the clock twice around the dial and the step twice
	// around the check.
	wantWire(t, res.check(t, CheckTCPConnect), `{"seq":4,"name":"tcp_connect","status":"ok","duration_ms":21,"detail":{"address":"cloud.example:80","rtt_ms":7}}`)
}

// A cancelled request stops the run: the interrupted check is not reported as
// a network failure and nothing after it runs.
func TestCancellationStopsWithoutReportingTheInterruptedCheck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := offlineRunner()
	r.Dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		cancel()
		<-ctx.Done()
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: ctx.Err()}
	}
	vendorRan := false
	logs := captureLog(t)
	res := run(ctx, r, Input{Kind: "k", Endpoint: "http://cloud.example", Vendor: []VendorCheck{{
		Name: CheckAuthRequest, Run: func(context.Context) (map[string]any, error) { vendorRan = true; return nil, nil },
	}}})
	if !errors.Is(res.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", res.err)
	}
	if want := []string{CheckEndpointFormat, CheckCredentialsShape, CheckDNSResolve}; !reflect.DeepEqual(res.names(), want) {
		t.Fatalf("checks = %v, want %v", res.names(), want)
	}
	if vendorRan {
		t.Fatal("vendor check ran after cancellation")
	}
	if logs.Len() != 0 {
		t.Fatalf("cancellation logged a failed check: %s", logs)
	}

	// Already cancelled: nothing is reported at all.
	res = run(ctx, offlineRunner(), Input{Kind: "k", Endpoint: "http://cloud.example"})
	if !errors.Is(res.err, context.Canceled) || len(res.checks) != 0 {
		t.Fatalf("pre-cancelled run = %v, %v", res.names(), res.err)
	}
}

// A run that outlives its deadline reports the check that was running, as it
// failed, and then ends with ErrRunTimeout instead of running the rest
// against an expired context.
func TestRunDeadlineEndsTheRun(t *testing.T) {
	r := offlineRunner()
	r.Timeout = 20 * time.Millisecond
	r.Dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: ctx.Err()}
	}
	captureLog(t)
	res := run(context.Background(), r, Input{Kind: "k", Endpoint: "http://cloud.example"})
	if !errors.Is(res.err, ErrRunTimeout) {
		t.Fatalf("err = %v, want ErrRunTimeout", res.err)
	}
	if want := []string{CheckEndpointFormat, CheckCredentialsShape, CheckDNSResolve, CheckTCPConnect}; !reflect.DeepEqual(res.names(), want) {
		t.Fatalf("checks = %v, want %v", res.names(), want)
	}
	if c := res.check(t, CheckTCPConnect); c.Status != StatusFailed || c.Code != messages.ErrorProviderTCPTimeout {
		t.Fatalf("tcp_connect = %+v", c)
	}
}

// The cause of a failed check goes to the server log, with the credential
// values removed, and never to the wire.
func TestFailedCheckIsLoggedRedacted(t *testing.T) {
	logs := captureLog(t)
	const secret = "sk-very-secret"
	in := Input{
		Kind: "runpod", EndpointNotApplicable: true,
		Redact: func(s string) string { return strings.ReplaceAll(s, secret, "[redacted]") },
		Vendor: []VendorCheck{{Name: CheckAuthRequest, Run: func(context.Context) (map[string]any, error) {
			return nil, &Failure{Code: messages.ErrorProviderAuthFailed, Hint: HintAuth,
				Err: errors.New("status 401: bad key " + secret)}
		}}},
	}
	res := run(context.Background(), offlineRunner(), in)
	c := res.check(t, CheckAuthRequest)
	wantWire(t, c, `{"seq":8,"name":"auth_request","status":"failed","duration_ms":0,"code":"PROVIDER_AUTH_FAILED","hint":"auth"}`)
	out := logs.String()
	for _, want := range []string{"kind=runpod", "check=auth_request", "code=PROVIDER_AUTH_FAILED", "status 401: bad key [redacted]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log misses %q: %s", want, out)
		}
	}
	if strings.Contains(out, secret) {
		t.Fatalf("secret reached the log: %s", out)
	}
}
