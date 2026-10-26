package apiclient

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func TestCloudHTTPSAuthenticatesWithTrustedCertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || !validTestAccessKey(r) {
			t.Error("HTTPS request did not retain TLS and AccessKey authentication")
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		fmt.Fprint(w, `{"inventories":[]}`)
	}))
	defer server.Close()
	for _, suffix := range []string{"", "/v1", "/zstack", "/zstack/v1/"} {
		t.Run(suffix, func(t *testing.T) {
			c, err := New(provider.Credentials{Endpoint: server.URL + suffix,
				PublicKey: "test-access-key", PrivateKey: "test-access-secret"})
			if err != nil {
				t.Fatal(err)
			}
			// Trust only the fixture CA, retaining hostname and certificate checks.
			transport := server.Client().Transport.(*http.Transport).Clone()
			defer transport.CloseIdleConnections()
			if transport.TLSClientConfig.InsecureSkipVerify {
				t.Fatal("test must verify the certificate")
			}
			c.http.Transport = transport
			var out struct{ Inventories []any }
			if err := c.Get(t.Context(), "/hosts", &out); err != nil || out.Inventories == nil {
				t.Fatalf("signed HTTPS query failed: %v", err)
			}
		})
	}
}

func TestCloudHTTPSRejectsUntrustedCertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("untrusted peer received an authenticated request")
	}))
	defer server.Close()
	c := clientForTest(t)
	c.endpoint = server.URL
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: x509.NewCertPool()}}
	defer transport.CloseIdleConnections()
	c.http.Transport = transport
	var out any
	err := c.Get(t.Context(), "/hosts", &out)
	apiErr := clientError(t, err, ErrorNetwork)
	var certificateError x509.UnknownAuthorityError
	if apiErr.Phase != PhaseTLS || !errors.As(err, &certificateError) {
		t.Fatalf("certificate failure lost TLS evidence: phase=%s err=%v", apiErr.Phase, err)
	}
}

func TestCloudHTTPSProxyDistinguishesTargetTLSAndCONNECTFailures(t *testing.T) {
	for _, tc := range []struct {
		name          string
		targetTLS     bool
		connectStatus int
		wantPhase     RequestPhase
	}{
		{"signed HTTPS success", true, http.StatusOK, ""},
		{"target TLS failure", false, http.StatusOK, PhaseTLS},
		{"proxy CONNECT rejected", true, http.StatusProxyAuthRequired, PhaseConnect},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.TLS == nil || r.URL.Path != "/zstack/v1/hosts" || !validTestAccessKey(r) {
					t.Error("proxy changed the signed HTTPS request")
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
				fmt.Fprint(w, `{"inventories":[]}`)
			}))
			if tc.targetTLS {
				target.StartTLS()
			} else {
				target.Start()
			}
			defer target.Close()
			proxy := cloudHTTPSProxyForTest(t, target.Listener.Addr().String(), tc.connectStatus)
			transport := proxy.Client().Transport.(*http.Transport).Clone()
			defer transport.CloseIdleConnections()
			proxyURL, err := url.Parse(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			transport.Proxy = http.ProxyURL(proxyURL)
			c := clientForTest(t)
			c.endpoint = strings.Replace(target.URL, "http://", "https://", 1) + "/zstack"
			c.http.Transport = transport
			var out struct{ Inventories []any }
			err = c.Get(t.Context(), "/hosts", &out)
			if tc.wantPhase == "" {
				if err != nil || out.Inventories == nil {
					t.Fatalf("HTTPS via proxy failed: %v", err)
				}
				return
			}
			apiErr := clientError(t, err, ErrorNetwork)
			if apiErr.Phase != tc.wantPhase || !apiErr.ViaProxy {
				t.Fatalf("phase=%s proxy=%v, want %s proxy=true", apiErr.Phase, apiErr.ViaProxy, tc.wantPhase)
			}
		})
	}
}

func cloudHTTPSProxyForTest(t *testing.T, target string, connectStatus int) *httptest.Server {
	t.Helper()
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != target {
			t.Error("unexpected proxy request")
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		if connectStatus != http.StatusOK {
			http.Error(w, "proxy denied CONNECT", connectStatus)
			return
		}
		upstream, err := net.DialTimeout("tcp", target, time.Second)
		if err != nil {
			t.Error(err)
			http.Error(w, "dial failed", http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		fmt.Fprint(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
		done := make(chan struct{})
		go func() {
			defer close(done)
			io.Copy(upstream, conn)
			upstream.Close()
		}()
		io.Copy(conn, upstream)
		conn.Close()
		<-done
	}))
	t.Cleanup(proxy.Close)
	return proxy
}
