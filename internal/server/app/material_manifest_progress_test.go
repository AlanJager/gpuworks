//go:build linux && dqlite

package app

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// This fixture uses actual member certificates and the production pinned TLS
// client. Only its timeout window is shortened; production keeps the 15s idle.
func materialManifestProgressPeer(t *testing.T, handler http.HandlerFunc, serverWriteTimeout ...time.Duration) (*http.Client, string) {
	t.Helper()
	now := time.Now().UTC()
	clusterID := strings.Repeat("b", 32)
	ca, key, err := identity.NewAuthority(clusterID, now)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := identity.CertificateFingerprint(ca)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := identity.LoadAuthority(ca, key, clusterID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := identity.NewTrust(ca, clusterID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	issue := func() (tls.Certificate, string) {
		key, err := identity.NewMemberKey()
		if err != nil {
			t.Fatal(err)
		}
		csr, err := identity.NewMemberCSR(key)
		if err != nil {
			t.Fatal(err)
		}
		fingerprint, err := identity.CSRKeyFingerprint(csr)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := authority.IssueMember(csr, fingerprint, "127.0.0.1:9910", now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		pair, err := tls.X509KeyPair(append(cert, ca...), key)
		if err != nil {
			t.Fatal(err)
		}
		return pair, fingerprint
	}
	serverPair, serverKey := issue()
	senderPair, _ := issue()
	serverTLS, err := enrollmentClientTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverPair}}, ca)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	if len(serverWriteTimeout) != 0 {
		server.Config.WriteTimeout = serverWriteTimeout[0]
	}
	server.TLS = serverTLS
	server.StartTLS()
	t.Cleanup(server.Close)
	client, err := memberMaterialClient(&enrollmentBundle{
		info: EnrollmentInfo{ClusterID: clusterID, CAFingerprint: pin},
		tls:  &tls.Config{Certificates: []tls.Certificate{senderPair}}, trust: trust,
	}, server.URL, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(*http.Transport)
	transport.ResponseHeaderTimeout = 200 * time.Millisecond
	dial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err == nil {
			conn.(*materialProgressConn).idle = 200 * time.Millisecond
		}
		return conn, err
	}
	t.Cleanup(client.CloseIdleConnections)
	return client, server.URL
}

func TestMaterialManifestProgressRenewsReceiverWriteDeadline(t *testing.T) {
	digest := materialTestDigest([]byte("manifest"))
	requests := newMaterialRequestTracker()
	t.Cleanup(requests.closeAdmissions)
	var progress atomic.Int32
	client, origin := materialManifestProgressPeer(t, requests.wrap(func(w http.ResponseWriter, r *http.Request) {
		for i := range 6 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(60 * time.Millisecond):
			}
			_ = materialTestDigest([]byte{byte(i)})
			writeMaterialManifestProgress(w, r, requests)
		}
		writeMaterialManifestReceipt(w, r, requests, 1, 2, digest, nil, "")
	}), 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{Got1xxResponse: func(int, textproto.MIMEHeader) error {
		progress.Add(1)
		return nil
	}})
	if err := materializeRemoteMaterialManifest(ctx, client, origin, digest, 1, 2); err != nil {
		t.Fatalf("receiver absolute write timer broke completed-stage progress: %v (progress=%d)", err, progress.Load())
	}
	if progress.Load() != 6 {
		t.Fatalf("receiver lost completed stages: %d", progress.Load())
	}
}

func TestMaterialManifestCompletedProgressOutlivesHeaderWindow(t *testing.T) {
	digest := materialTestDigest([]byte("manifest"))
	var progress atomic.Int32
	client, origin := materialManifestProgressPeer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != materialManifestPath+"/"+digest+"/materialize" || r.ContentLength != 0 || len(r.TLS.VerifiedChains) == 0 {
			t.Error("unexpected or unauthenticated manifest request")
		}
		if r.Header.Get(materialManifestProgressHeader) != "1" {
			t.Error("manifest client did not opt into completed-stage progress")
		}
		for i := range 6 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(60 * time.Millisecond):
			}
			// A digest computation finishes before each progress frame. There
			// is no independent keepalive while application work is stopped.
			_ = materialTestDigest([]byte{byte(i)})
			w.WriteHeader(http.StatusProcessing)
		}
		w.Header().Set("X-Gpuworks-Material-Source", "1")
		w.Header().Set("X-Gpuworks-Material-Member", "2")
		w.Header().Set("X-Gpuworks-Material-Digest", digest)
		w.WriteHeader(http.StatusNoContent)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
		if code != http.StatusProcessing || header.Get("X-Gpuworks-Material-Digest") != "" {
			t.Error("progress carried a premature receipt")
		}
		progress.Add(1)
		return nil
	}})
	started := time.Now()
	if err := materializeRemoteMaterialManifest(ctx, client, origin, digest, 1, 2); err != nil {
		t.Fatalf("completed stages did not keep the manifest request alive: %v (progress=%d)", err, progress.Load())
	}
	if progress.Load() != 6 || time.Since(started) <= 200*time.Millisecond {
		t.Fatal("request did not cross the absolute header window with real progress")
	}
	if client.Transport.(*http.Transport).ResponseHeaderTimeout != 200*time.Millisecond {
		t.Fatal("manifest call changed the shared transport timeout")
	}
}

func TestMaterialManifestProgressOnlyCommitsFinalReceipt(t *testing.T) {
	for _, mode := range []string{"legacy", "opt-in", "confirm-failure", "confirm-cancel", "owner-close"} {
		t.Run(mode, func(t *testing.T) {
			digest := materialTestDigest([]byte("manifest"))
			requests := newMaterialRequestTracker()
			t.Cleanup(requests.closeAdmissions)
			client, origin := materialManifestProgressPeer(t, requests.wrap(func(w http.ResponseWriter, r *http.Request) {
				ctx, cancel := context.WithCancel(r.Context())
				defer cancel()
				r = r.WithContext(ctx)
				writeMaterialManifestProgress(w, r, requests)
				writeMaterialManifestReceipt(w, r, requests, 1, 2, digest, func() error {
					if mode == "confirm-cancel" {
						cancel()
					}
					if mode == "owner-close" {
						requests.closeAdmissions()
					}
					// Exercise progress while the receipt headers are staged.
					writeMaterialManifestProgress(w, r, requests)
					if mode == "confirm-failure" {
						return errors.New("consumer changed")
					}
					return nil
				}, "consumer changed")
			}))
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			var progress atomic.Int32
			ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
				if code != http.StatusProcessing {
					t.Errorf("unexpected progress status: %d", code)
				}
				for _, key := range []string{"X-Gpuworks-Material-Source", "X-Gpuworks-Material-Member", "X-Gpuworks-Material-Digest"} {
					if header.Get(key) != "" {
						t.Errorf("progress carried uncommitted %s: %v", key, header)
					}
				}
				progress.Add(1)
				return nil
			}})
			r, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/materialize", nil)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "legacy" {
				r.Header.Set(materialManifestProgressHeader, "1")
			}
			response, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			wantProgress := int32(2)
			if mode == "legacy" {
				wantProgress = 0
			}
			if mode == "confirm-cancel" || mode == "owner-close" {
				wantProgress = 1
			}
			if progress.Load() != wantProgress {
				t.Fatalf("unexpected progress after completed/stopped stage: got=%d want=%d", progress.Load(), wantProgress)
			}
			if mode == "legacy" || mode == "opt-in" {
				if response.StatusCode != http.StatusNoContent || response.Header.Get("X-Gpuworks-Material-Source") != "1" || response.Header.Get("X-Gpuworks-Material-Member") != "2" || response.Header.Get("X-Gpuworks-Material-Digest") != digest {
					t.Fatalf("final receipt lost its binding: HTTP %d %v", response.StatusCode, response.Header)
				}
			} else {
				if response.StatusCode != http.StatusServiceUnavailable {
					t.Fatalf("interrupted confirmation committed HTTP %d", response.StatusCode)
				}
				for _, key := range []string{"X-Gpuworks-Material-Source", "X-Gpuworks-Material-Member", "X-Gpuworks-Material-Digest"} {
					if response.Header.Get(key) != "" {
						t.Errorf("failed final response retained receipt %s", key)
					}
				}
			}
		})
	}
}

func TestMaterialManifestProgressStillFailsClosed(t *testing.T) {
	for _, mode := range []string{"stalled", "canceled", "wrong-source", "wrong-member", "wrong-digest", "failed-status"} {
		t.Run(mode, func(t *testing.T) {
			digest := materialTestDigest([]byte("manifest"))
			var final atomic.Bool
			client, origin := materialManifestProgressPeer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusProcessing)
				if mode == "stalled" || mode == "canceled" {
					<-r.Context().Done()
					return
				}
				w.Header().Set("X-Gpuworks-Material-Source", "1")
				w.Header().Set("X-Gpuworks-Material-Member", "2")
				w.Header().Set("X-Gpuworks-Material-Digest", digest)
				switch mode {
				case "wrong-source":
					w.Header().Set("X-Gpuworks-Material-Source", "3")
				case "wrong-member":
					w.Header().Set("X-Gpuworks-Material-Member", "3")
				case "wrong-digest":
					w.Header().Set("X-Gpuworks-Material-Digest", materialTestDigest([]byte("other")))
				case "failed-status":
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				final.Store(true)
				w.WriteHeader(http.StatusNoContent)
			})
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			var progress atomic.Int32
			ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{Got1xxResponse: func(int, textproto.MIMEHeader) error {
				progress.Add(1)
				if mode == "canceled" {
					cancel()
				}
				return nil
			}})
			started := time.Now()
			err := materializeRemoteMaterialManifest(ctx, client, origin, digest, 1, 2)
			if err == nil || progress.Load() != 1 {
				t.Fatalf("invalid final proof accepted: %v progress=%d", err, progress.Load())
			}
			if mode == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel did not propagate: %v", err)
			}
			if mode == "stalled" && (time.Since(started) > time.Second || final.Load()) {
				t.Fatalf("stalled work escaped idle timeout: %v", err)
			}
		})
	}
}
