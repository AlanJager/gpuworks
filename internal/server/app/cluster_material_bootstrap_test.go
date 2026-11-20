//go:build linux && dqlite

package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Offline HTTP fixtures retain real cluster certificates, exact installed-route
// metadata and production full-byte/consumer checks. They do not claim native
// grants; the native receiver tests own that separate part of the contract.
func bootstrapMaterialFixture(t *testing.T, fault string, entered chan struct{}) (*enrollmentBundle, []store.ClusterTransportMember, []store.MaterialManifestRecord, int, *atomic.Int32) {
	t.Helper()
	now := time.Now().UTC()
	clusterID := strings.Repeat("a", 32)
	ca, caKey, err := identity.NewAuthority(clusterID, now)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := identity.CertificateFingerprint(ca)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := identity.LoadAuthority(ca, caKey, clusterID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := identity.NewTrust(ca, clusterID, pin, now)
	if err != nil {
		t.Fatal(err)
	}
	first, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Fatal(err)
	}
	port := first.Addr().(*net.TCPAddr).Port
	second, err := net.Listen("tcp", net.JoinHostPort("127.0.0.3", strconv.Itoa(port)))
	if err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	listeners := []net.Listener{first, second}
	t.Cleanup(func() { _ = first.Close(); _ = second.Close() })
	issue := func(host string) (tls.Certificate, string) {
		t.Helper()
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
		cert, err := authority.IssueMember(csr, fingerprint, net.JoinHostPort(host, strconv.Itoa(port)), now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		pair, err := tls.X509KeyPair(append(cert, ca...), key)
		if err != nil {
			t.Fatal(err)
		}
		pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		return pair, fingerprint
	}
	var pairs []tls.Certificate
	var members []store.ClusterTransportMember
	for i, host := range []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"} {
		pair, key := issue(host)
		pairs = append(pairs, pair)
		members = append(members, store.ClusterTransportMember{ID: uint64(11 + i), Address: net.JoinHostPort(host, "9001"), Role: "voter", KeyFingerprint: key, EnrollmentOrigin: "https://" + net.JoinHostPort(host, strconv.Itoa(port))})
	}
	config, err := enrollmentClientTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pairs[0]}}, ca)
	if err != nil {
		t.Fatal(err)
	}
	bundle := &enrollmentBundle{info: EnrollmentInfo{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion, PublicURL: members[0].EnrollmentOrigin}, tls: config, trust: trust}
	body := []byte("shared immutable master bytes")
	fileDigest := materialTestDigest(body)
	manifest := materialManifest{Version: materialManifestVersion, ClusterID: clusterID, SourceMemberID: 11, SourceKey: members[0].KeyFingerprint, Classes: slices.Clone(requiredMaterialClasses), Files: []materialManifestFile{{Class: MaterialMasterSecret, Path: "master.key", SHA256: fileDigest, Bytes: int64(len(body))}}}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	digest := materialTestDigest(raw)
	pointers := make([]store.MaterialManifestRecord, 3)
	for i, member := range members {
		pointers[i] = store.MaterialManifestRecord{SourceMemberID: int64(member.ID), ClusterID: clusterID, SourceKey: member.KeyFingerprint, Digest: fmt.Sprintf("%064x", i+1), Bytes: 128, RecordedAt: now}
	}
	pointers[0].Digest, pointers[0].Bytes = digest, int64(len(raw))
	requests := &atomic.Int32{}
	for i, listener := range listeners {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "material-replicas"), 0700); err != nil {
			t.Fatal(err)
		}
		receiver, err := newMaterialReceiver(filepath.Join(root, "material-replicas"), members[i+1].ID)
		if err != nil {
			t.Fatal(err)
		}
		for file, bytes := range map[string][]byte{filepath.Join(receiver.root, digest[:2], digest): raw, filepath.Join(receiver.root, fileDigest[:2], fileDigest): body, filepath.Join(root, "master.key"): body} {
			if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, bytes, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if i == 0 && fault == "consumer-incomplete" {
			_ = os.Remove(filepath.Join(root, "master.key"))
		}
		if i == 0 && fault == "replica-corrupt" {
			if err := os.WriteFile(filepath.Join(receiver.root, fileDigest[:2], fileDigest), []byte("wrong same-length material bytes"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			if r.Method != http.MethodPost || r.URL.Path != materialManifestPath+"/"+digest+"/installed" || r.URL.RawQuery != "" || r.ContentLength > 0 || r.Header.Get("Authorization") != "" {
				http.Error(w, "invalid installed request", http.StatusBadRequest)
				return
			}
			if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
				http.Error(w, "member authentication required", http.StatusUnauthorized)
				return
			}
			key, err := trust.VerifyMember(r.TLS.PeerCertificates[0], time.Now().UTC())
			if err != nil || key != members[0].KeyFingerprint {
				http.Error(w, "material source rejected", http.StatusForbidden)
				return
			}
			if fault == "http-cancel" {
				close(entered)
				<-r.Context().Done()
				return
			}
			if err := verifyReceivedMaterial(r.Context(), filepath.Join(receiver.root, digest[:2], digest), digest, int64(len(raw))); err != nil {
				http.Error(w, "manifest unavailable", http.StatusServiceUnavailable)
				return
			}
			if err := verifyReceivedMaterial(r.Context(), filepath.Join(receiver.root, fileDigest[:2], fileDigest), fileDigest, int64(len(body))); err != nil {
				http.Error(w, "replica unavailable", http.StatusServiceUnavailable)
				return
			}
			if err := checkInstalledMaterialManifest(r.Context(), receiver, manifest); err != nil {
				http.Error(w, "consumer unavailable", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("X-Gpuworks-Material-Source", "11")
			w.Header().Set("X-Gpuworks-Material-Member", fmt.Sprint(receiver.memberID))
			w.Header().Set("X-Gpuworks-Material-Digest", digest)
			if fault == "wrong-receipt" {
				w.Header().Set("X-Gpuworks-Material-Member", "99")
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		_ = server.Listener.Close()
		server.Listener = listener
		pair := pairs[i+1]
		if i == 0 && fault == "wrong-certificate" {
			pair, _ = issue("127.0.0.2")
		}
		server.TLS, err = enrollmentClientTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}, ca)
		if err != nil {
			t.Fatal(err)
		}
		server.StartTLS()
		t.Cleanup(server.Close)
	}
	return bundle, members, pointers, port, requests
}

func TestClusterMaterialBootstrapUsesInstalledChecksBetweenClosedSnapshots(t *testing.T) {
	for _, fault := range []string{"", "missing-pointer", "consumer-incomplete", "replica-corrupt", "wrong-receipt", "wrong-certificate", "foreign-origin", "foreign-database", "phase-change", "pointer-change", "member-change", "snapshot-cleanup", "http-cancel"} {
		t.Run(fault, func(t *testing.T) {
			entered := make(chan struct{})
			bundle, members, pointers, port, requests := bootstrapMaterialFixture(t, fault, entered)
			phase := store.MaintenancePhaseState{Phase: store.MaintenanceServing, Revision: 1}
			reads := 0
			read := func(ctx context.Context, expected store.ClusterIdentity, key string) (store.MaintenancePhaseState, []store.ClusterTransportMember, []store.MaterialManifestRecord, error) {
				reads++
				if expected.ClusterID != bundle.info.ClusterID || key != members[0].KeyFingerprint || requests.Load() != int32((reads-1)*2) {
					t.Error("snapshot/HTTP order or identity changed")
				}
				if fault == "snapshot-cleanup" {
					return store.MaintenancePhaseState{}, nil, nil, errors.New("actual snapshot cleanup failed")
				}
				m, p, state := slices.Clone(members), slices.Clone(pointers), phase
				if fault == "missing-pointer" {
					p = p[:2]
				}
				if fault == "foreign-origin" {
					m[1].EnrollmentOrigin = "https://127.0.0.9:" + strconv.Itoa(port)
				}
				if fault == "foreign-database" {
					m[1].Address = "127.0.0.9:9001"
				}
				if reads == 2 {
					switch fault {
					case "phase-change":
						state.Revision++
					case "pointer-change":
						p[2].Digest = strings.Repeat("f", 64)
					case "member-change":
						m[2].KeyFingerprint = strings.Repeat("f", 64)
					}
				}
				return state, m, p, ctx.Err()
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- checkClusterMaterialBootstrap(ctx, bundle, "127.0.0.1", 11, []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"}, 9001, port, read)
			}()
			if fault == "http-cancel" {
				select {
				case <-entered:
					cancel()
				case <-time.After(time.Second):
					t.Fatal("real installed HTTP did not start")
				}
			}
			select {
			case err := <-done:
				if (err == nil) != (fault == "") {
					t.Fatalf("bootstrap fault=%s err=%v reads=%d HTTP=%d", fault, err, reads, requests.Load())
				}
				if fault == "" && (reads != 2 || requests.Load() != 2) {
					t.Fatalf("did not verify both installed peers: reads=%d HTTP=%d", reads, requests.Load())
				}
				if fault == "http-cancel" && !errors.Is(err, context.Canceled) {
					t.Fatal("HTTP cancellation cause lost", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("bootstrap operation did not finish")
			}
		})
	}
}

func TestClusterMaterialBootstrapMissingLocalBundleDoesNotReadOrInitialize(t *testing.T) {
	root := t.TempDir()
	calls := 0
	read := func(context.Context, store.ClusterIdentity, string) (store.MaintenancePhaseState, []store.ClusterTransportMember, []store.MaterialManifestRecord, error) {
		calls++
		return store.MaintenancePhaseState{}, nil, nil, errors.New("missing identity reached SQL")
	}
	err := CheckClusterMaterialBootstrap(t.Context(), Config{DataDir: root, DBPath: filepath.Join(root, "gpuworks.db")}, "127.0.0.1", 11, []string{"127.0.0.1", "127.0.0.2", "127.0.0.3"}, 9001, 9443, read)
	entries, listErr := os.ReadDir(root)
	if err == nil || calls != 0 || listErr != nil || len(entries) != 0 {
		t.Fatalf("missing bundle was initialized or accepted: err=%v reads=%d entries=%v listErr=%v", err, calls, entries, listErr)
	}
}
