package apiclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func TestCloudAccessKeyRejectionsPreserveSafeReasons(t *testing.T) {
	for _, tc := range []struct {
		name, details string
		want          AuthenticationReason
	}{
		{"missing or disabled key", "access key id: secret-access-key does not existed or disabled", AuthAccessKeyInvalid},
		{"signature", "wrong accessKey signature", AuthSignatureInvalid},
		{"license", "AccessKey authentication requires valid Enterprise license", AuthLicenseRequired},
		{"clock", "requestTimeTooSkewed", AuthClockSkew},
		{"unknown denial", "private remote details secret-access-key", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := clientForTest(t, WithReadRetries(2))
			calls := 0
			c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return cloudResponse(r, 403, fmt.Sprintf(`{"error":{"code":"SYS.1006","description":"An operation failed","details":%q},"diagnostic":{"code":"ORG_ZSTACK_CORE_REST_10015"}}`, tc.details)), nil
			})
			var out any
			err := c.Get(t.Context(), "/hosts", &out)
			apiErr := clientError(t, err, ErrorAuthentication)
			if apiErr.AuthReason != tc.want || calls != 1 || apiErr.Code != "SYS.1006" || apiErr.DiagnosticCode != "ORG_ZSTACK_CORE_REST_10015" {
				t.Fatalf("wrong denial classification: %+v, calls=%d", apiErr, calls)
			}
			if strings.Contains(fmt.Sprintf("%+v", apiErr), "secret-access-key") {
				t.Fatal("remote credentials escaped in the error")
			}
		})
	}
}

func TestCloudConnectionPhaseSurvivesTimeoutWrapping(t *testing.T) {
	for _, phase := range []RequestPhase{PhaseConnect, PhaseTLS, PhaseResponse} {
		t.Run(string(phase), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := clientForTest(t, WithRequestTimeout(time.Second), WithOperationTimeout(2*time.Second))
				cause := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ETIMEDOUT}
				c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
					trace := httptrace.ContextClientTrace(r.Context())
					trace.ConnectStart("tcp", "127.0.0.1:8080")
					if phase == PhaseTLS {
						trace.TLSHandshakeStart()
					}
					if phase == PhaseResponse {
						trace.GotConn(httptrace.GotConnInfo{Reused: true})
					}
					<-r.Context().Done()
					return nil, cause
				})
				var out any
				err := c.Get(t.Context(), "/hosts", &out)
				apiErr := clientError(t, err, ErrorTimeout)
				if apiErr.Phase != phase || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, cause) {
					t.Fatalf("lost phase or original cause: phase=%s err=%v", apiErr.Phase, err)
				}
			})
		})
	}
}

func TestCloudRealHTTPTimeoutIsResponseFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	c, err := New(provider.Credentials{Endpoint: server.URL, PublicKey: "ak", PrivateKey: "sk"}, WithRequestTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	var out any
	apiErr := clientError(t, c.Get(t.Context(), "/hosts", &out), ErrorTimeout)
	if apiErr.Phase != PhaseResponse || apiErr.StatusCode != 200 {
		t.Fatalf("body timeout classified as disconnected: %+v", apiErr)
	}
}

func TestCloudConnectionTraceIsSafeForConcurrentCallbacks(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://cloud.example", nil)
	req, phase := traceConnection(req)
	trace := httptrace.ContextClientTrace(req.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			trace.ConnectStart("tcp", "address")
			trace.TLSHandshakeStart()
			trace.TLSHandshakeDone(tls.ConnectionState{}, nil)
			trace.GotConn(httptrace.GotConnInfo{})
		}
	}()
	for range 100 {
		_ = phase()
	}
	<-done
	trace.ConnectStart("tcp", "late alternate address")
	if phase() != PhaseResponse {
		t.Fatal("successful connection lost its phase")
	}
}

func TestCloudProxyConnectionFailureKeepsProxyEvidence(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	proxy, err := url.Parse("http://private-proxy-user:private-proxy-password@" + address)
	if err != nil {
		t.Fatal(err)
	}
	c := clientForTest(t)
	c.http.Transport = &http.Transport{Proxy: http.ProxyURL(proxy)}
	var out any
	apiErr := clientError(t, c.Get(t.Context(), "/hosts", &out), ErrorNetwork)
	if !apiErr.ViaProxy || apiErr.Phase != PhaseConnect || !errors.Is(apiErr, syscall.ECONNREFUSED) {
		t.Fatalf("proxy failure lost evidence: %+v", apiErr)
	}
	if strings.Contains(apiErr.Error(), "private-proxy") {
		t.Fatal("proxy credentials leaked")
	}
}
