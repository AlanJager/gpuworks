package outbound

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestTransportRejectsUnicodeHostnameBeforeDNS(t *testing.T) {
	for _, hostname := range []string{"bücher.example", "模型.example", "bu\u0308cher.example"} {
		t.Run(hostname, func(t *testing.T) {
			transport := Transport(func() (proto.NetworkConfig, error) { return proto.NetworkConfig{}, nil }, hostname)
			defer transport.CloseIdleConnections()
			var lookups atomic.Int32
			transport.policy.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
				lookups.Add(1)
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			_, err := (&http.Client{Transport: transport}).Get("http://" + hostname + "/model")
			if err == nil || !errors.Is(err, ErrTargetDenied) || !strings.Contains(err.Error(), "ASCII/Punycode") || lookups.Load() != 0 {
				t.Fatalf("Unicode host did not fail validation before DNS: err=%v lookups=%d", err, lookups.Load())
			}
		})
	}
}

func TestTransportPunycodeHostnamePreservesHTTPAuthorityAndUnicodePath(t *testing.T) {
	const hostname = "xn--bcher-kva.example"
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "HTTP", true: "HTTPS"}[secure], func(t *testing.T) {
			var requests atomic.Int32
			origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if !strings.HasPrefix(r.Host, hostname+":") || r.URL.Path != "/模型" {
					t.Errorf("authority or path changed: Host=%q path=%q", r.Host, r.URL.Path)
				}
				if secure && r.TLS.ServerName != hostname {
					t.Errorf("SNI=%q, want %q", r.TLS.ServerName, hostname)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			var roots *x509.CertPool
			if secure {
				key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				certificate := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{hostname},
					NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
					ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
				der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
				if err != nil {
					t.Fatal(err)
				}
				parsed, err := x509.ParseCertificate(der)
				if err != nil {
					t.Fatal(err)
				}
				roots = x509.NewCertPool()
				roots.AddCert(parsed)
				origin.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
				origin.StartTLS()
			} else {
				origin.Start()
			}
			defer origin.Close()
			parsed, _ := url.Parse(origin.URL)
			transport := Transport(func() (proto.NetworkConfig, error) { return proto.NetworkConfig{}, nil }, hostname)
			transport.Proxy = nil
			if secure {
				transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
			}
			defer transport.CloseIdleConnections()
			var lookups atomic.Int32
			transport.policy.lookup = func(_ context.Context, _, host string) ([]netip.Addr, error) {
				lookups.Add(1)
				if host != hostname {
					t.Errorf("DNS host=%q, want %q", host, hostname)
				}
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			}
			client := &http.Client{Transport: transport}
			for range 2 {
				resp, err := client.Get(parsed.Scheme + "://" + hostname + ":" + parsed.Port() + "/模型")
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusNoContent {
					t.Fatalf("status=%d", resp.StatusCode)
				}
			}
			if requests.Load() != 2 || lookups.Load() != 2 {
				t.Fatalf("requests=%d DNS checks=%d", requests.Load(), lookups.Load())
			}
		})
	}
}
