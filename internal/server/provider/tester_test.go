package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider/testconn"
	"gpuworks.ai/gpuworks/internal/server/store"
)

var testerNow = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// testerDriver is a driver under a chosen kind name whose credential
// validation can fail and which records the credentials it was asked about.
// It brings no vendor checks of its own.
type testerDriver struct {
	serviceTestDriver
	kind        string
	validateErr error
	validated   []Credentials
}

func (d *testerDriver) Name() string { return d.kind }

func (d *testerDriver) ValidateCredentials(_ context.Context, creds Credentials) error {
	d.validated = append(d.validated, creds)
	return d.validateErr
}

// vendorDriver brings its own vendor checks, as the ZStack driver does.
type vendorDriver struct {
	testerDriver
	checks func(Credentials) []testconn.VendorCheck
}

func (d *vendorDriver) ConnectionChecks(creds Credentials) []testconn.VendorCheck {
	return d.checks(creds)
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// testerNet is the network a hermetic runner sees: which addresses were
// dialed, and whether the dial succeeds.
type testerNet struct {
	dialErr error
	dialed  []string
}

// runner reaches nothing real: the resolver answers a documentation address,
// a dial hands back one end of an in-memory pipe (or dialErr), and the
// clock-skew request is answered in process with the runner's own time.
func (n *testerNet) runner() *testconn.Runner {
	return &testconn.Runner{
		Now:        func() time.Time { return testerNow },
		LookupHost: func(context.Context, string) ([]string, error) { return []string{"192.0.2.10"}, nil },
		Dial: func(_ context.Context, _, address string) (net.Conn, error) {
			n.dialed = append(n.dialed, address)
			if n.dialErr != nil {
				return nil, n.dialErr
			}
			client, server := net.Pipe()
			_ = server.Close()
			return client, nil
		},
		Proxy:  func(*http.Request) (*url.URL, error) { return nil, nil },
		Getenv: func(string) string { return "" },
		HTTPClient: &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     http.Header{"Date": []string{testerNow.Format(http.TimeFormat)}},
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		})},
	}
}

func testerService(t *testing.T, drv Driver, network *testerNet) *Service {
	t.Helper()
	svc := NewService(nil, nil)
	svc.SetDriver(drv)
	svc.SetConnectionTestRunner(network.runner())
	return svc
}

type testerRun struct {
	checks  []testconn.Check
	summary testconn.Summary
}

func runTest(t *testing.T, svc *Service, kind string, creds Credentials) testerRun {
	t.Helper()
	var res testerRun
	var err error
	res.summary, err = svc.TestConnection(context.Background(), kind, creds, func(c testconn.Check) {
		res.checks = append(res.checks, c)
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// frames is the run exactly as a client receives it, one check per line.
func (res testerRun) frames(t *testing.T) []string {
	t.Helper()
	out := make([]string, 0, len(res.checks))
	for _, c := range res.checks {
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return out
}

func wantFrames(t *testing.T, res testerRun, want []string) {
	t.Helper()
	got := res.frames(t)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("frames\n got %s\nwant %s", strings.Join(got, "\n     "), strings.Join(want, "\n     "))
	}
}

func captureTesterLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

// The five generic frames of a reachable plain-HTTP endpoint.
var reachableFrames = []string{
	`{"seq":1,"name":"endpoint_format","status":"ok","duration_ms":0,"detail":{"host":"cloud.example:8080","scheme":"http"}}`,
	`{"seq":2,"name":"credentials_shape","status":"ok","duration_ms":0,"detail":{"fields":1}}`,
	`{"seq":3,"name":"dns_resolve","status":"ok","duration_ms":0,"detail":{"ips":["192.0.2.10"]}}`,
	`{"seq":4,"name":"tcp_connect","status":"ok","duration_ms":0,"detail":{"address":"cloud.example:8080","rtt_ms":0}}`,
	`{"seq":5,"name":"tls_handshake","status":"ok","duration_ms":0,"detail":{"mode":"plain_http"}}`,
	`{"seq":6,"name":"proxy_detect","status":"ok","duration_ms":0,"detail":{"proxy":"none"}}`,
	`{"seq":7,"name":"clock_skew","status":"ok","duration_ms":0,"detail":{"skew_seconds":0}}`,
}

// A driver without vendor checks of its own gets one: its credential
// validation call, with the credentials under test.
func TestTestConnectionGenericAuthRequest(t *testing.T) {
	creds := Credentials{Endpoint: "http://cloud.example:8080", APIKey: "key-under-test"}
	t.Run("accepted", func(t *testing.T) {
		drv := &testerDriver{kind: "tester-kind"}
		res := runTest(t, testerService(t, drv, &testerNet{}), "tester-kind", creds)
		wantFrames(t, res, append(append([]string{}, reachableFrames...),
			`{"seq":8,"name":"auth_request","status":"ok","duration_ms":0}`))
		if !res.summary.OK || res.summary.Failed != nil {
			t.Fatalf("summary = %+v", res.summary)
		}
		if !reflect.DeepEqual(drv.validated, []Credentials{creds}) {
			t.Fatalf("validated %+v, want the credentials under test once", drv.validated)
		}
	})
	t.Run("rejected", func(t *testing.T) {
		logs := captureTesterLog(t)
		drv := &testerDriver{kind: "tester-kind",
			validateErr: errors.New(`list pods: status 401: {"error":"vendor says key-under-test is revoked"}`)}
		res := runTest(t, testerService(t, drv, &testerNet{}), "tester-kind", creds)
		wantFrames(t, res, append(append([]string{}, reachableFrames...),
			`{"seq":8,"name":"auth_request","status":"failed","duration_ms":0,"code":"PROVIDER_AUTH_FAILED","hint":"auth"}`))
		if !reflect.DeepEqual(res.summary, testconn.Summary{Failed: []string{testconn.CheckAuthRequest}}) {
			t.Fatalf("summary = %+v", res.summary)
		}
		// The driver's error text stays off the wire and reaches the log with
		// the credential value removed.
		for _, frame := range res.frames(t) {
			if strings.Contains(frame, "vendor says") || strings.Contains(frame, "key-under-test") {
				t.Fatalf("driver error text reached the wire: %s", frame)
			}
		}
		out := logs.String()
		if !strings.Contains(out, "check=auth_request") || !strings.Contains(out, "code=PROVIDER_AUTH_FAILED") ||
			!strings.Contains(out, "status 401") || !strings.Contains(out, "[redacted] is revoked") {
			t.Fatalf("failed check not logged with its cause: %s", out)
		}
		if strings.Contains(out, "key-under-test") {
			t.Fatalf("credential value reached the log: %s", out)
		}
	})
	t.Run("skipped behind an unreachable endpoint", func(t *testing.T) {
		captureTesterLog(t)
		drv := &testerDriver{kind: "tester-kind"}
		res := runTest(t, testerService(t, drv, &testerNet{dialErr: syscall.ECONNREFUSED}), "tester-kind", creds)
		got := res.frames(t)
		if got[3] != `{"seq":4,"name":"tcp_connect","status":"failed","duration_ms":0,"code":"PROVIDER_TCP_REFUSED","hint":"tcp_refused"}` {
			t.Fatalf("tcp_connect frame = %s", got[3])
		}
		if got[7] != `{"seq":8,"name":"auth_request","status":"skipped","duration_ms":0,"detail":{"depends":"tcp_connect"},"hint":"depends_failed"}` {
			t.Fatalf("auth_request frame = %s", got[7])
		}
		if len(drv.validated) != 0 {
			t.Fatal("credentials were validated against an unreachable endpoint")
		}
		if !reflect.DeepEqual(res.summary, testconn.Summary{Failed: []string{testconn.CheckTCPConnect}}) {
			t.Fatalf("summary = %+v", res.summary)
		}
	})
	t.Run("skipped behind a missing credential", func(t *testing.T) {
		captureTesterLog(t)
		// tester-kind has no schema of its own: the static table requires
		// api_key.
		drv := &testerDriver{kind: "tester-kind"}
		res := runTest(t, testerService(t, drv, &testerNet{}), "tester-kind", Credentials{Endpoint: creds.Endpoint})
		got := res.frames(t)
		if got[1] != `{"seq":2,"name":"credentials_shape","status":"failed","duration_ms":0,"code":"PROVIDER_CREDENTIAL_MISSING","detail":{"field":"api_key"},"hint":"credential_missing"}` {
			t.Fatalf("credentials_shape frame = %s", got[1])
		}
		if got[7] != `{"seq":8,"name":"auth_request","status":"skipped","duration_ms":0,"detail":{"depends":"credentials_shape"},"hint":"depends_failed"}` {
			t.Fatalf("auth_request frame = %s", got[7])
		}
		if len(drv.validated) != 0 {
			t.Fatal("credentials were validated with a required field missing")
		}
	})
}

// A driver's own vendor checks replace the generic one and run in its order:
// ok, failed with its code and hint, skipped behind the failed one.
func TestTestConnectionRunsVendorChecks(t *testing.T) {
	captureTesterLog(t)
	var seen []Credentials
	drv := &vendorDriver{testerDriver: testerDriver{kind: "vendor-kind"}}
	drv.checks = func(creds Credentials) []testconn.VendorCheck {
		seen = append(seen, creds)
		return []testconn.VendorCheck{
			{Name: testconn.CheckAuthRequest, Depends: []string{testconn.CheckTCPConnect},
				Run: func(context.Context) (map[string]any, error) { return map[string]any{"auth": "access_key"}, nil }},
			{Name: testconn.CheckReadPermissions, Depends: []string{testconn.CheckAuthRequest},
				Run: func(context.Context) (map[string]any, error) {
					return nil, &testconn.Failure{Code: messages.ErrorZStackAuthRejected, Hint: "zstack_permission",
						Detail: map[string]any{"denied": []string{"hosts"}}, Err: errors.New("denied")}
				}},
			{Name: testconn.CheckResourceSummary, Depends: []string{testconn.CheckReadPermissions},
				Run: func(context.Context) (map[string]any, error) {
					t.Error("resource_summary ran behind a failed read_permissions")
					return nil, nil
				}},
		}
	}
	creds := Credentials{Endpoint: "http://cloud.example:8080", APIKey: "k"}
	res := runTest(t, testerService(t, drv, &testerNet{}), "vendor-kind", creds)
	wantFrames(t, res, append(append([]string{}, reachableFrames...),
		`{"seq":8,"name":"auth_request","status":"ok","duration_ms":0,"detail":{"auth":"access_key"}}`,
		`{"seq":9,"name":"read_permissions","status":"failed","duration_ms":0,"code":"ZSTACK_AUTH_REJECTED","detail":{"denied":["hosts"]},"hint":"zstack_permission"}`,
		`{"seq":10,"name":"resource_summary","status":"skipped","duration_ms":0,"detail":{"depends":"read_permissions"},"hint":"depends_failed"}`))
	if !reflect.DeepEqual(res.summary, testconn.Summary{Failed: []string{testconn.CheckReadPermissions}}) {
		t.Fatalf("summary = %+v", res.summary)
	}
	if !reflect.DeepEqual(seen, []Credentials{creds}) {
		t.Fatalf("vendor checks built for %+v", seen)
	}
	if len(drv.validated) != 0 {
		t.Fatal("the generic credential check ran beside the driver's own checks")
	}
}

func TestTestConnectionUnknownKind(t *testing.T) {
	svc := NewService(nil, nil)
	svc.SetConnectionTestRunner((&testerNet{}).runner())
	reported := 0
	if _, err := svc.TestConnection(context.Background(), "nope", Credentials{}, func(testconn.Check) { reported++ }); err == nil {
		t.Fatal("unknown kind must error")
	}
	if reported != 0 {
		t.Fatalf("unknown kind reported %d checks", reported)
	}
}

// An empty endpoint is the kind's default one from the single table the
// credential form's default value also reads.
func TestTestConnectionEmptyEndpointUsesKindDefault(t *testing.T) {
	cases := map[string]string{
		"autodl":       "api.autodl.com",
		"compshare":    "api.compshare.cn",
		"digitalocean": "api.digitalocean.com",
		"runpod":       "api.runpod.io",
		"vast":         "console.vast.ai",
	}
	for kind, host := range cases {
		t.Run(kind, func(t *testing.T) {
			captureTesterLog(t)
			if got := defaultEndpoint(kind); !strings.HasPrefix(got, "https://"+host) {
				t.Fatalf("defaultEndpoint(%s) = %q", kind, got)
			}
			network := &testerNet{dialErr: syscall.ECONNREFUSED}
			res := runTest(t, testerService(t, &testerDriver{kind: kind}, network), kind,
				Credentials{APIKey: "k", PublicKey: "pk", PrivateKey: "sk"})
			if got, want := res.frames(t)[0], `{"seq":1,"name":"endpoint_format","status":"ok","duration_ms":0,"detail":{"host":"`+host+`","scheme":"https"}}`; got != want {
				t.Fatalf("endpoint_format frame\n got %s\nwant %s", got, want)
			}
			if !reflect.DeepEqual(network.dialed, []string{host + ":443"}) {
				t.Fatalf("dialed %v, want the default endpoint", network.dialed)
			}
			// An explicit endpoint still wins over the default.
			network.dialed = nil
			runTest(t, testerService(t, &testerDriver{kind: kind}, network), kind,
				Credentials{Endpoint: "http://override.example:8080/", APIKey: "k", PublicKey: "pk", PrivateKey: "sk"})
			if !reflect.DeepEqual(network.dialed, []string{"override.example:8080"}) {
				t.Fatalf("dialed %v, want the override", network.dialed)
			}
		})
	}
}

// aliyun's schema has no endpoint field and no default endpoint: the driver
// picks regional endpoints itself. Its endpoint and network checks are not
// applicable, and its credentials are still verified.
func TestTestConnectionKindWithoutEndpoint(t *testing.T) {
	creds := Credentials{PublicKey: "ak", PrivateKey: "sk", Extra: map[string]string{"regions": "cn-hangzhou"}}
	frames := func(auth string) []string {
		return []string{
			`{"seq":1,"name":"endpoint_format","status":"skipped","duration_ms":0,"hint":"not_applicable"}`,
			`{"seq":2,"name":"credentials_shape","status":"ok","duration_ms":0,"detail":{"fields":2}}`,
			`{"seq":3,"name":"dns_resolve","status":"skipped","duration_ms":0,"hint":"not_applicable"}`,
			`{"seq":4,"name":"tcp_connect","status":"skipped","duration_ms":0,"hint":"not_applicable"}`,
			`{"seq":5,"name":"tls_handshake","status":"skipped","duration_ms":0,"hint":"not_applicable"}`,
			`{"seq":6,"name":"proxy_detect","status":"skipped","duration_ms":0,"hint":"not_applicable"}`,
			`{"seq":7,"name":"clock_skew","status":"skipped","duration_ms":0,"hint":"not_applicable"}`,
			auth,
		}
	}
	t.Run("accepted", func(t *testing.T) {
		drv := &testerDriver{kind: "aliyun"}
		network := &testerNet{}
		res := runTest(t, testerService(t, drv, network), "aliyun", creds)
		wantFrames(t, res, frames(`{"seq":8,"name":"auth_request","status":"ok","duration_ms":0}`))
		if !res.summary.OK {
			t.Fatalf("summary = %+v", res.summary)
		}
		if len(network.dialed) != 0 {
			t.Fatalf("dialed %v with no endpoint to reach", network.dialed)
		}
		if !reflect.DeepEqual(drv.validated, []Credentials{creds}) {
			t.Fatalf("validated %+v", drv.validated)
		}
	})
	t.Run("rejected", func(t *testing.T) {
		captureTesterLog(t)
		drv := &testerDriver{kind: "aliyun", validateErr: errors.New("InvalidAccessKeyId.NotFound")}
		res := runTest(t, testerService(t, drv, &testerNet{}), "aliyun", creds)
		wantFrames(t, res, frames(`{"seq":8,"name":"auth_request","status":"failed","duration_ms":0,"code":"PROVIDER_AUTH_FAILED","hint":"auth"}`))
		if !reflect.DeepEqual(res.summary, testconn.Summary{Failed: []string{testconn.CheckAuthRequest}}) {
			t.Fatalf("summary = %+v", res.summary)
		}
	})
	// A kind that does take an endpoint and was given none fails
	// endpoint_format instead: "not applicable" is only for kinds without the
	// field.
	t.Run("endpoint field left empty", func(t *testing.T) {
		captureTesterLog(t)
		drv := &testerDriver{kind: "tester-kind"}
		res := runTest(t, testerService(t, drv, &testerNet{}), "tester-kind", Credentials{APIKey: "k"})
		if got, want := res.frames(t)[0], `{"seq":1,"name":"endpoint_format","status":"failed","duration_ms":0,"code":"PROVIDER_ENDPOINT_INVALID","hint":"endpoint_empty"}`; got != want {
			t.Fatalf("endpoint_format frame = %s", got)
		}
		if len(drv.validated) != 0 {
			t.Fatal("credentials validated without an endpoint")
		}
	})
}

func TestCredentialRedactor(t *testing.T) {
	redact := credentialRedactor(Credentials{
		Endpoint: "http://cloud.example:8080", APIKey: "abc", PublicKey: "abcdef", PrivateKey: " sk-1 ",
		Extra: map[string]string{"ssh_password": "hunter2", "empty": "  "},
	})
	got := redact(`Get "http://cloud.example:8080/x": abcdef abc sk-1 hunter2 done`)
	if want := `Get "http://cloud.example:8080/x": [redacted] [redacted] [redacted] [redacted] done`; got != want {
		t.Fatalf("redacted = %q, want %q", got, want)
	}
	if got := credentialRedactor(Credentials{})("unchanged"); got != "unchanged" {
		t.Fatalf("empty credentials changed the text: %q", got)
	}
}

func savedTesterService(t *testing.T, drv Driver, network *testerNet) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := auth.NewSecretbox("provider-tester-saved-test")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, box)
	svc.SetDriver(drv)
	svc.SetConnectionTestRunner(network.runner())
	return svc, st
}

// A stored provider is tested with its stored credentials; a non-empty
// override replaces one field for the test only, and a blank secret keeps the
// stored value, as an update does.
func TestTestSavedConnection(t *testing.T) {
	drv := &testerDriver{kind: "tester-kind"}
	network := &testerNet{}
	svc, _ := savedTesterService(t, drv, network)
	stored := Credentials{Endpoint: "http://cloud.example:8080", APIKey: "stored-key", Extra: map[string]string{"zone": "a"}}
	p, err := svc.Add(context.Background(), "tester-kind", "saved", stored)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	test := func(overrides Credentials) (testerRun, Credentials) {
		t.Helper()
		drv.validated, network.dialed = nil, nil
		var res testerRun
		res.summary, err = svc.TestSavedConnection(context.Background(), p.ID, overrides, func(c testconn.Check) {
			res.checks = append(res.checks, c)
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(drv.validated) != 1 {
			t.Fatalf("validated %d times, want once", len(drv.validated))
		}
		return res, drv.validated[0]
	}

	res, used := test(Credentials{})
	if !reflect.DeepEqual(used, stored) {
		t.Fatalf("no overrides: validated %+v, want the stored credentials", used)
	}
	wantFrames(t, res, append(append([]string{}, reachableFrames...),
		`{"seq":8,"name":"auth_request","status":"ok","duration_ms":0}`))

	_, used = test(Credentials{Endpoint: "http://other.example:9090", APIKey: "  ", Extra: map[string]string{"zone": "", "pool": "p1"}})
	want := Credentials{Endpoint: "http://other.example:9090", APIKey: "stored-key", Extra: map[string]string{"zone": "a", "pool": "p1"}}
	if !reflect.DeepEqual(used, want) {
		t.Fatalf("endpoint override: validated %+v, want %+v", used, want)
	}
	if !reflect.DeepEqual(network.dialed, []string{"other.example:9090"}) {
		t.Fatalf("dialed %v, want the overriding endpoint", network.dialed)
	}

	_, used = test(Credentials{APIKey: "typed-key"})
	if used.APIKey != "typed-key" || used.Endpoint != stored.Endpoint {
		t.Fatalf("secret override: validated %+v", used)
	}

	// The overrides are for one test: the stored credentials are unchanged.
	_, used = test(Credentials{})
	if !reflect.DeepEqual(used, stored) {
		t.Fatalf("after overrides: validated %+v, want the stored credentials", used)
	}
}

func TestTestSavedConnectionErrors(t *testing.T) {
	drv := &testerDriver{kind: "tester-kind"}
	svc, st := savedTesterService(t, drv, &testerNet{})
	p, err := svc.Add(context.Background(), "tester-kind", "saved", Credentials{Endpoint: "http://cloud.example:8080", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	drv.validated = nil
	noReport := func(c testconn.Check) { t.Errorf("unexpected check %s", c.Name) }

	if _, err := svc.TestSavedConnection(context.Background(), p.ID+1000, Credentials{}, noReport); err == nil {
		t.Fatal("unknown provider id must error")
	}

	// Stored credentials that no longer decrypt.
	otherBox, err := auth.NewSecretbox("another-secret")
	if err != nil {
		t.Fatal(err)
	}
	broken := &Service{store: st, box: otherBox}
	broken.SetDriver(drv)
	broken.SetConnectionTestRunner((&testerNet{}).runner())
	if _, err := broken.TestSavedConnection(context.Background(), p.ID, Credentials{}, noReport); err == nil {
		t.Fatal("undecryptable credentials must error")
	}

	// A stored provider whose driver is no longer registered.
	orphan := &Service{store: st, box: svc.box}
	orphan.SetConnectionTestRunner((&testerNet{}).runner())
	if _, err := orphan.TestSavedConnection(context.Background(), p.ID, Credentials{}, noReport); err == nil {
		t.Fatal("unregistered driver must error")
	}
	if len(drv.validated) != 0 {
		t.Fatalf("credentials validated on an error path: %+v", drv.validated)
	}
}

// A cancelled request ends the run with the context's error instead of a
// network failure.
func TestTestConnectionCancelled(t *testing.T) {
	logs := captureTesterLog(t)
	ctx, cancel := context.WithCancel(context.Background())
	drv := &testerDriver{kind: "tester-kind"}
	svc := NewService(nil, nil)
	svc.SetDriver(drv)
	runner := (&testerNet{}).runner()
	runner.Dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	svc.SetConnectionTestRunner(runner)
	var names []string
	_, err := svc.TestConnection(ctx, "tester-kind", Credentials{Endpoint: "http://cloud.example:8080", APIKey: "k"}, func(c testconn.Check) {
		names = append(names, c.Name)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if want := []string{"endpoint_format", "credentials_shape", "dns_resolve"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("reported %v, want %v", names, want)
	}
	if len(drv.validated) != 0 || logs.Len() != 0 {
		t.Fatalf("cancelled run validated %d times, logged %q", len(drv.validated), logs)
	}
}

// The service uses the real runner unless one is injected.
func TestConnectionTestRunnerDefaultsToReal(t *testing.T) {
	svc := NewService(nil, nil)
	if r := svc.connectionTestRunner(); r == nil || r.Dial == nil || r.Proxy == nil || r.LookupHost == nil {
		t.Fatalf("default runner = %+v", r)
	}
	injected := (&testerNet{}).runner()
	svc.SetConnectionTestRunner(injected)
	if svc.connectionTestRunner() != injected {
		t.Fatal("injected runner not used")
	}
}
