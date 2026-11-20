package enrollment

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type fixture struct {
	ca           []byte
	caKey        []byte
	issuer       *identity.Authority
	cluster, pin string
	server       *httptest.Server
}

func endpoint(t *testing.T, handler http.Handler) *fixture {
	t.Helper()
	f := &fixture{cluster: strings.Repeat("a", 32)}
	var err error
	var key []byte
	f.ca, key, err = identity.NewAuthority(f.cluster, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f.caKey = key
	f.pin, err = identity.CertificateFingerprint(f.ca)
	if err != nil {
		t.Fatal(err)
	}
	f.issuer, err = identity.LoadAuthority(f.ca, key, f.cluster, f.pin, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	key, err = identity.NewMemberKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := identity.NewMemberCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := identity.CSRKeyFingerprint(csr)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := f.issuer.IssueMember(csr, fp, "127.0.0.1:9443", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(append(cert, f.ca...), key)
	if err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewUnstartedServer(handler)
	f.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	return f
}

func (f *fixture) client(t *testing.T) *Client {
	t.Helper()
	c, err := NewClient(f.server.URL+certificatePath, f.cluster, f.pin, proto.ClusterProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestClientRealAPIExchangeReplayAndResponseLoss(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "enrollment.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var router http.Handler
	var drop atomic.Bool
	f := endpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if drop.Swap(false) {
			// Commit through the real handler, then lose the transport response.
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, r)
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = connection.Close()
			}
			return
		}
		router.ServeHTTP(w, r)
	}))
	if _, err := st.InitializeClusterIdentity(t.Context(), store.ClusterIdentity{ClusterID: f.cluster, CAFingerprint: f.pin, Protocol: proto.ClusterProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	admin := store.User{Username: "admin", Role: "admin"}
	if err := st.DB().Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	router, err = api.ClusterEnrollmentRouter(t.Context(), api.ClusterEnrollmentDeps{Store: st, Signer: signer, Issuer: f.issuer, PublicURL: f.server.URL})
	if err != nil {
		t.Fatal(err)
	}
	c := f.client(t)
	for index, lose := range []bool{false, true} {
		_, token, err := st.IssueClusterInvitation(t.Context(), f.cluster, admin.ID, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		keyDir := t.TempDir()
		if err := os.Chmod(keyDir, 0700); err != nil {
			t.Fatal(err)
		}
		key, err := identity.CreateMemberKey(filepath.Join(keyDir, "member.key"))
		if err != nil {
			t.Fatal(err)
		}
		address := []string{"NODE.EXAMPLE.:09010", "127.0.0.1:9011"}[index]
		drop.Store(lose)
		result, err := c.Exchange(t.Context(), token, key, address)
		if lose && err == nil {
			t.Fatal("lost response reported success")
		}
		if !lose && err != nil {
			t.Fatal(err)
		}
		if !lose && result.Address != "node.example:9010" {
			t.Fatal("address not normalized")
		}
		recovered, err := c.Recover(t.Context(), key, address)
		if err != nil || recovered.CertificatePEM == "" {
			t.Fatalf("recover: %v", err)
		}
		if _, err := c.Exchange(t.Context(), token, key, address); err == nil {
			t.Fatal("invitation replay accepted")
		}
		other, _ := identity.NewMemberKey()
		if _, err := c.Recover(t.Context(), other, address); err == nil {
			t.Fatal("unbound key recovered another admission")
		}
	}
	var count int64
	if err := st.DB().Model(&store.ClusterAdmission{}).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("response loss/recovery duplicated admissions: count=%d err=%v", count, err)
	}
}

func TestClientRejectsPeerBeforeCredentialAndNeverRedirects(t *testing.T) {
	var requests atomic.Int32
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(100) }))
	defer redirect.Close()
	f := endpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, redirect.URL, http.StatusTemporaryRedirect)
	}))
	key, _ := identity.NewMemberKey()
	wrong, err := NewClient(f.server.URL+certificatePath, f.cluster, strings.Repeat("0", 64), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	if _, err := wrong.Exchange(t.Context(), "secret", key, "node.example:9010"); err == nil {
		t.Fatal("wrong CA accepted")
	}
	if requests.Load() != 0 {
		t.Fatal("invitation sent before server authentication")
	}
	if _, err := f.client(t).Exchange(t.Context(), "secret", key, "node.example:9010"); err == nil {
		t.Fatal("redirect accepted")
	}
	if requests.Load() != 1 {
		t.Fatal("redirect forwarded credentials")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.client(t).Exchange(ctx, "secret", key, "node.example:9010"); err == nil {
		t.Fatal("canceled request succeeded")
	}
	if requests.Load() != 1 {
		t.Fatal("canceled request reached endpoint")
	}
}

func TestClientRejectsInvalidResponseWithoutEchoingSecrets(t *testing.T) {
	for _, scenario := range []string{"wrong cluster", "wrong key", "wrong protocol", "wrong address", "wrong authority", "other member key", "other certificate host", "extra certificate", "bad certificate", "oversized", "trailing", "remote error"} {
		t.Run(scenario, func(t *testing.T) {
			var f *fixture
			f = endpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var input dto.ClusterCertificateRequest
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
					http.Error(w, "invalid", 400)
					return
				}
				fp, err := identity.CSRKeyFingerprint([]byte(input.CSRPEM))
				if err != nil {
					t.Error(err)
					return
				}
				cert, err := f.issuer.IssueMember([]byte(input.CSRPEM), fp, input.Address, time.Now(), time.Hour)
				if err != nil {
					t.Error(err)
					http.Error(w, "invalid CSR", 400)
					return
				}
				result := dto.ClusterCertificateResponse{ClusterID: f.cluster, Protocol: 1, Address: input.Address, KeyFingerprint: fp, CertificatePEM: string(cert), CACertificatePEM: string(f.ca)}
				if scenario == "remote error" {
					http.Error(w, r.Header.Get("Authorization"), 503)
					return
				}
				w.WriteHeader(201)
				switch scenario {
				case "wrong cluster":
					result.ClusterID = strings.Repeat("b", 32)
				case "wrong key":
					result.KeyFingerprint = strings.Repeat("0", 64)
				case "wrong protocol":
					result.Protocol++
				case "wrong address":
					result.Address = "other.example:9010"
				case "wrong authority":
					ca, _, err := identity.NewAuthority(f.cluster, time.Now())
					if err != nil {
						t.Error(err)
						return
					}
					result.CACertificatePEM = string(ca)
				case "other member key":
					key, err := identity.NewMemberKey()
					if err != nil {
						t.Error(err)
						return
					}
					csr, err := identity.NewMemberCSR(key)
					if err != nil {
						t.Error(err)
						return
					}
					fingerprint, err := identity.CSRKeyFingerprint(csr)
					if err != nil {
						t.Error(err)
						return
					}
					cert, err := f.issuer.IssueMember(csr, fingerprint, input.Address, time.Now(), time.Hour)
					if err != nil {
						t.Error(err)
						return
					}
					result.CertificatePEM = string(cert)
				case "other certificate host":
					cert, err := f.issuer.IssueMember([]byte(input.CSRPEM), fp, "other.example:9010", time.Now(), time.Hour)
					if err != nil {
						t.Error(err)
						return
					}
					result.CertificatePEM = string(cert)
				case "bad certificate":
					result.CertificatePEM = "invalid"
				case "extra certificate":
					result.CertificatePEM += string(f.ca)
				case "oversized":
					_, _ = io.WriteString(w, strings.Repeat("x", maxCertificateResponse+1))
					return
				}
				_ = json.NewEncoder(w).Encode(result)
				if scenario == "trailing" {
					_, _ = io.WriteString(w, "{}")
				}
			}))
			key, _ := identity.NewMemberKey()
			result, err := f.client(t).Exchange(t.Context(), "private-invitation-token", key, "node.example:9010")
			if err == nil || result.CertificatePEM != "" {
				t.Fatal("invalid remote result returned as usable certificate")
			}
			if strings.Contains(err.Error(), "private-invitation-token") {
				t.Fatal("remote error reflected credential")
			}
		})
	}
}

func TestClientRejectsAmbiguousEndpointAndProtocol(t *testing.T) {
	for _, endpoint := range []string{
		"http://example.com" + certificatePath,
		"https://secret@example.com" + certificatePath,
		"https://example.com" + certificatePath + "?token=secret",
		"https://example.com" + certificatePath + "#fragment",
		"https://example.com/another-path",
		"https://example.com:0" + certificatePath,
	} {
		if client, err := NewClient(endpoint, strings.Repeat("a", 32), strings.Repeat("b", 64), 1); err == nil {
			client.Close()
			t.Fatal("ambiguous or insecure endpoint accepted")
		}
	}
	if client, err := NewClient("https://example.com"+certificatePath, strings.Repeat("a", 32), strings.Repeat("b", 64), 2); err == nil {
		client.Close()
		t.Fatal("unknown protocol accepted")
	}
}

func TestClientCancellationInterruptsExchange(t *testing.T) {
	entered := make(chan struct{})
	f := endpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
	}))
	c := f.client(t)
	key, _ := identity.NewMemberKey()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Exchange(ctx, "secret", key, "node.example:9010"); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not reach TLS endpoint")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled request succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request ignored cancellation")
	}
}
