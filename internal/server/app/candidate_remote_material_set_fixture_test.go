//go:build linux && dqlite

package app

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type candidateRemoteSetFixture struct {
	t                    *testing.T
	root                 string
	store                *store.Store
	memberID             uint64
	clusterID, senderKey string
	client               *http.Client
	server               *httptest.Server
	mu                   sync.Mutex
	requests             [][]materialBatchItem
	beforeServe          func(http.ResponseWriter, *http.Request) bool
	handler              http.Handler
}

func newCandidateRemoteSetFixture(t *testing.T, memberID uint64) *candidateRemoteSetFixture {
	t.Helper()
	now := time.Now().UTC()
	clusterID := strings.Repeat("b", 32)
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
	issue := func(address string) (tls.Certificate, string) {
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
		cert, err := authority.IssueMember(csr, fingerprint, address, now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		pair, err := tls.X509KeyPair(append(cert, ca...), key)
		if err != nil {
			t.Fatal(err)
		}
		return pair, fingerprint
	}
	serverPair, serverKey := issue("127.0.0.1:9910")
	senderPair, senderKey := issue("127.0.0.1:9911")
	serverTLS, err := enrollmentClientTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverPair}}, ca)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "material.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.InitializeClusterIdentity(t.Context(), store.ClusterIdentity{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InitializeClusterSeedAdmission(t.Context(), clusterID, serverKey, "127.0.0.1:9910"); err != nil {
		t.Fatal(err)
	}
	admin := store.User{Username: "admin", Role: "admin"}
	if err := st.DB().Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	_, token, err := st.IssueClusterInvitation(t.Context(), clusterID, admin.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimClusterInvitation(t.Context(), clusterID, token, senderKey, "127.0.0.1:9911"); err != nil {
		t.Fatal(err)
	}
	f := &candidateRemoteSetFixture{store: st, t: t, root: t.TempDir(), memberID: memberID, clusterID: clusterID, senderKey: senderKey}
	receiver, err := newMaterialReceiver(f.root, memberID)
	if err != nil {
		t.Fatal(err)
	}
	bundle := &enrollmentBundle{info: EnrollmentInfo{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion}, tls: serverTLS, trust: trust}
	handler := enrollmentMaterialHandlerWithReceiver(bundle, st, http.NotFoundHandler(), nil, receiver)
	f.handler = handler
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var items []materialBatchItem
		if r.Method == http.MethodPost && r.URL.Path == materialBatchPath {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "request read failed", http.StatusBadRequest)
				return
			}
			_ = r.Body.Close()
			var request materialBatchRequest
			if err := json.Unmarshal(raw, &request); err != nil {
				http.Error(w, "request invalid", http.StatusBadRequest)
				return
			}
			items = request.Files
			r.Body = io.NopCloser(bytes.NewReader(raw))
		} else if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, materialReplicaPath+"/") {
			items = []materialBatchItem{{Digest: strings.TrimPrefix(r.URL.Path, materialReplicaPath+"/")}}
		} else {
			http.Error(w, "unexpected method or route", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.requests = append(f.requests, slices.Clone(items))
		before := f.beforeServe
		f.mu.Unlock()
		if before != nil && before(w, r) {
			return
		}
		handler.ServeHTTP(w, r)
	}))
	f.server.TLS = serverTLS
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	sender := *bundle
	sender.tls = &tls.Config{Certificates: []tls.Certificate{senderPair}}
	f.client, err = memberMaterialClient(&sender, f.server.URL, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.client.CloseIdleConnections)
	return f
}

func (f *candidateRemoteSetFixture) put(body []byte) materialBatchItem {
	f.t.Helper()
	digest := materialTestDigest(body)
	if err := os.MkdirAll(filepath.Join(f.root, digest[:2]), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, digest[:2], digest), body, 0600); err != nil {
		f.t.Fatal(err)
	}
	return materialBatchItem{Digest: digest, Bytes: int64(len(body))}
}

func (f *candidateRemoteSetFixture) source(sourceID uint64, files []materialBatchItem) (store.MaterialManifestRecord, materialManifest) {
	f.t.Helper()
	manifest := materialManifest{Version: materialManifestVersion, ClusterID: f.clusterID, SourceMemberID: sourceID, SourceKey: f.senderKey, Classes: slices.Clone(requiredMaterialClasses)}
	for i, file := range files {
		manifest.Files = append(manifest.Files, materialManifestFile{Class: MaterialOfflineImage, Path: "images/" + file.Digest + ".tar", SHA256: file.Digest, Bytes: file.Bytes})
		if i == 0 {
			manifest.Files[0].Class = MaterialMasterSecret
			manifest.Files[0].Path = "master.key"
		}
	}
	slices.SortFunc(manifest.Files, func(a, b materialManifestFile) int {
		if a.Class != b.Class {
			return strings.Compare(string(a.Class), string(b.Class))
		}
		return strings.Compare(a.Path, b.Path)
	})
	raw, err := json.Marshal(manifest)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := parseMaterialManifest(raw, store.ClusterIdentity{ClusterID: f.clusterID}, sourceID, f.senderKey); err != nil {
		f.t.Fatal(err)
	}
	pointer := f.put(raw)
	return store.MaterialManifestRecord{SourceMemberID: int64(sourceID), ClusterID: f.clusterID, SourceKey: f.senderKey, Digest: pointer.Digest, Bytes: pointer.Bytes}, manifest
}

func (f *candidateRemoteSetFixture) takeRequests() [][]materialBatchItem {
	f.mu.Lock()
	defer f.mu.Unlock()
	requests := f.requests
	f.requests = nil
	return requests
}

func assertCandidateRemoteSetReads(t *testing.T, requests [][]materialBatchItem, expected map[string]int) {
	t.Helper()
	actual := make(map[string]int)
	for _, request := range requests {
		for _, item := range request {
			actual[item.Digest]++
		}
	}
	if !maps.Equal(actual, expected) {
		t.Fatalf("actual reads %v, want %v (requests=%v)", actual, expected, requests)
	}
}
