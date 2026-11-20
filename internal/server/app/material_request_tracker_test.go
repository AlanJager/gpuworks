//go:build linux && dqlite

package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestMaterialRequestTrackerWithdrawsOnlyAfterHandlerReturns(t *testing.T) {
	tracker := newMaterialRequestTracker()
	entered := make(chan struct{})
	release := make(chan struct{})
	handler := tracker.wrap(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := tracker.withdraw(ctx); err != context.DeadlineExceeded {
		t.Fatalf("withdraw completed while a handler was active: %v", err)
	}
	rejected := httptest.NewRecorder()
	handler(rejected, httptest.NewRequest(http.MethodGet, "/", nil))
	if rejected.Code != http.StatusServiceUnavailable {
		t.Fatalf("withdrawn tracker admitted another request: HTTP %d", rejected.Code)
	}
	close(release)
	<-finished
	if err := tracker.withdraw(t.Context()); err != nil {
		t.Fatalf("completed handler did not release tracker: %v", err)
	}
	canceled, stop := context.WithCancel(t.Context())
	stop()
	handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil).WithContext(canceled))
	if err := tracker.withdraw(t.Context()); err != nil {
		t.Fatalf("canceled request held tracker: %v", err)
	}
}

func TestMaterialRoutesRejectFrozenRequestsBeforeBytesOrFiles(t *testing.T) {
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
	issue := func(address string) (*x509.Certificate, string) {
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
		cert, err := authority.IssueMember(csr, fingerprint, address, now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		pair, err := tls.X509KeyPair(append(cert, ca...), key)
		if err != nil {
			t.Fatal(err)
		}
		return pair.Leaf, fingerprint
	}
	_, seedKey := issue("127.0.0.1:9910")
	peerCert, peerKey := issue("127.0.0.1:9911")
	st, err := store.Open(filepath.Join(t.TempDir(), "materials.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.InitializeClusterIdentity(t.Context(), store.ClusterIdentity{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InitializeClusterSeedAdmission(t.Context(), clusterID, seedKey, "127.0.0.1:9910"); err != nil {
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
	if _, err := st.ClaimClusterInvitation(t.Context(), clusterID, token, peerKey, "127.0.0.1:9911"); err != nil {
		t.Fatal(err)
	}
	receiver, err := newMaterialReceiver(t.TempDir(), 7)
	if err != nil {
		t.Fatal(err)
	}
	bundle := &enrollmentBundle{info: EnrollmentInfo{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion}, trust: trust}
	router := chi.NewRouter()
	requests := newMaterialRequestTracker()
	registerEnrollmentMaterialReceiver(router, bundle, st, receiver, requests)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if method == http.MethodPost && strings.HasPrefix(path, materialReplicaPath+"/") {
			r.Header.Set("Content-Type", "application/octet-stream")
		}
		r.TLS = &tls.ConnectionState{HandshakeComplete: true, PeerCertificates: []*x509.Certificate{peerCert}, VerifiedChains: [][]*x509.Certificate{{peerCert}}}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	digest := strings.Repeat("b", 64)
	if got := request(http.MethodGet, materialReplicaPath+"/"+digest, ""); got.Code != http.StatusNotFound {
		t.Fatalf("serving member failed to enter material read route: HTTP %d", got.Code)
	}
	before, err := st.ClusterAdmissionForVerifiedKey(t.Context(), clusterID, peerKey)
	if err != nil || before.MaterialGrantSequence != 1 {
		t.Fatalf("serving request did not commit a grant: %+v %v", before, err)
	}
	serving, err := st.ReadMaintenancePhase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// A request admitted while serving can finish copying just as freezing
	// commits. Its later index write must fail, and no success receipt may leave.
	inFlightBody := "file completed before its index was rejected"
	inFlightDigest := materialTestDigest([]byte(inFlightBody))
	parent := filepath.Join(receiver.root, inFlightDigest[:2])
	transitioned := false
	receiver.syncDir = func(path string) error {
		if path == parent && !transitioned {
			transitioned = true
			if _, err := st.TransitionMaintenancePhase(t.Context(), serving, store.MaintenanceFreezing); err != nil {
				return err
			}
		}
		return syncMaterialDir(path)
	}
	if got := request(http.MethodPost, materialReplicaPath+"/"+inFlightDigest, inFlightBody); got.Code != http.StatusServiceUnavailable || strings.Contains(got.Body.String(), `"digest"`) || !transitioned {
		t.Fatalf("in-flight frozen upload returned a receipt: HTTP %d body=%q transition=%t", got.Code, got.Body.String(), transitioned)
	}
	var indexed int64
	if err := st.DB().Model(&store.MaterialReplica{}).Where("digest = ? AND member_id = ?", inFlightDigest, 7).Count(&indexed).Error; err != nil || indexed != 0 {
		t.Fatalf("rejected upload left a replica index: count=%d err=%v", indexed, err)
	}
	if got, err := os.ReadFile(filepath.Join(parent, inFlightDigest)); err != nil || string(got) != inFlightBody {
		t.Fatalf("rejected index removed durable content-addressed bytes: %q %v", got, err)
	}
	before, err = st.ClusterAdmissionForVerifiedKey(t.Context(), clusterID, peerKey)
	if err != nil || before.MaterialGrantSequence != 2 {
		t.Fatalf("in-flight request lacked its pre-freeze grant: %+v %v", before, err)
	}
	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, materialReplicaPath + "/" + digest, ""},
		{http.MethodHead, materialReplicaPath + "/" + digest, ""},
		{http.MethodPost, materialReplicaPath + "/" + digest, "material body"},
		{http.MethodPost, materialBatchPath, `{}`},
		{http.MethodPost, materialManifestPath + "/" + digest + "/verify", ""},
		{http.MethodPost, materialManifestPath + "/" + digest + "/materialize", ""},
		{http.MethodPost, materialManifestPath + "/" + digest + "/installed", ""},
	} {
		got := request(route.method, route.path, route.body)
		if got.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s after freeze: HTTP %d, want 503", route.method, route.path, got.Code)
		}
	}
	after, err := st.ClusterAdmissionForVerifiedKey(t.Context(), clusterID, peerKey)
	if err != nil || after.MaterialGrantSequence != before.MaterialGrantSequence {
		t.Fatalf("frozen routes advanced grant sequence: before=%d after=%d err=%v", before.MaterialGrantSequence, after.MaterialGrantSequence, err)
	}
	if _, err := os.Stat(filepath.Join(receiver.root, digest[:2], digest)); !os.IsNotExist(err) {
		t.Fatalf("frozen upload published a file: %v", err)
	}
	if err := requests.withdraw(t.Context()); err != nil {
		t.Fatalf("completed frozen requests remained in flight: %v", err)
	}
}
