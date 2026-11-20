//go:build linux && dqlite

package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func joinedMaterialAssemblyFixture(t *testing.T) (*App, *enrollmentBundle) {
	t.Helper()
	a := joinedClassificationApp(t, nil)
	a.legacyClosed = false
	st, err := store.Open(filepath.Join(a.cfg.DataDir, "assembly.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	a.store = st
	t.Cleanup(func() { _ = a.Close() })
	now, clusterID := time.Now().UTC(), strings.Repeat("a", 32)
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
	certificate, err := authority.IssueMember(csr, fingerprint, "127.0.0.1:9910", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(append(certificate, ca...), key)
	if err != nil {
		t.Fatal(err)
	}
	pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	config, err := enrollmentClientTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}, ca)
	if err != nil {
		t.Fatal(err)
	}
	bundle := &enrollmentBundle{info: EnrollmentInfo{ClusterID: clusterID, CAFingerprint: pin,
		Protocol: store.ClusterProtocolVersion, ListenAddress: "127.0.0.1:0", PublicURL: "https://127.0.0.1:9910"},
		issuer: authority, trust: trust, tls: config, ca: ca, dataDir: a.cfg.DataDir}
	if _, err := st.InitializeClusterIdentity(t.Context(), store.ClusterIdentity{ClusterID: clusterID,
		CAFingerprint: pin, Protocol: store.ClusterProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.ClusterAdmission{KeyFingerprint: fingerprint, Address: "127.0.0.1:9910"}).Error; err != nil {
		t.Fatal(err)
	}
	st.NoteClusterSessionRoute(store.ClusterSessionRoute{ID: 7, Key: []byte("local-key"), Listen: "127.0.0.1:9910"})
	return a, bundle
}

func TestJoinedMaterialAssemblyFailureRetriesWithoutDuplicateBarrierOrListener(t *testing.T) {
	a, bundle := joinedMaterialAssemblyFixture(t)
	gate, st, b := a.businessHTTP, a.store, a.store.Bus
	bundle.info.ListenAddress = ""
	if err := a.assembleClosedMemberMaterials(t.Context(), bundle, false); err == nil {
		t.Fatal("invalid enrollment listener accepted")
	}
	if a.enrollment != nil || a.businessHTTP != gate || a.store != st || a.store.Bus != b {
		t.Fatal("failed material assembly replaced borrowed owners")
	}
	assertAssemblyStoreLive(t, a)
	// The failure follows receiver construction. A barrier installed before
	// this failure would reject this real ordinary writer and poison retry.
	if err := fencedBusinessWrite(st); err != nil {
		t.Fatalf("partial failure installed business barrier: %v", err)
	}
	bundle.info.ListenAddress = "127.0.0.1:0"
	if err := a.assembleClosedMemberMaterials(t.Context(), bundle, false); err != nil {
		t.Fatal(err)
	}
	original := a.enrollment
	receiver, requests := original.materialReceiver, original.materialRequests
	if original == nil || receiver == nil || requests == nil || a.businessHTTP != gate {
		t.Fatal("retry did not retain one gate and material owner")
	}
	if err := fencedBusinessWrite(st); !errors.Is(err, store.ErrBusinessWritesClosed) {
		t.Fatal(err)
	}
	if err := a.prepareJoinedMaterialRuntime(t.Context()); err != nil {
		t.Fatal(err)
	}
	if a.enrollment != original || original.materialReceiver != receiver || original.materialRequests != requests {
		t.Fatal("repeated preparation replaced material owner")
	}
	if err := original.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	shutdown, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := original.Close(shutdown); err != nil {
		t.Fatal(err)
	}
}

func TestJoinedMaterialAssemblyCanceledAdmissionDoesNotInitialize(t *testing.T) {
	a, bundle := joinedMaterialAssemblyFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.assembleClosedMemberMaterials(ctx, bundle, false); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if a.enrollment != nil {
		t.Fatal("canceled preparation published material owner")
	}
	if _, err := os.Stat(filepath.Join(a.cfg.DataDir, "material-replicas")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	assertAssemblyStoreLive(t, a)
}

func TestJoinedMaterialAssemblyClassifiedFailureRetainsBarrierForRetry(t *testing.T) {
	a, bundle := joinedMaterialAssemblyFixture(t)
	st, gate := a.store, a.businessHTTP
	a.store = nil
	s := &classifiedJoinedSession{classify: func(context.Context, *bus.Bus) (*store.Store, bool, error) {
		return st, false, nil
	}}
	a.joinedRaft = s
	if err := a.classifyJoinedStore(t.Context()); err != nil {
		t.Fatal(err)
	}
	bundle.info.ListenAddress = ""
	if err := a.assembleClosedMemberMaterials(t.Context(), bundle, false); err == nil {
		t.Fatal("classified partial assembly accepted invalid listener")
	}
	if a.enrollment != nil || a.store != st || a.businessHTTP != gate || s.closes.Load() != 0 {
		t.Fatal("partial failure discarded classified owners or published listener")
	}
	if err := fencedBusinessWrite(st); !errors.Is(err, store.ErrBusinessWritesClosed) {
		t.Fatalf("partial assembly reopened original barrier: %v", err)
	}
	bundle.info.ListenAddress = "127.0.0.1:0"
	if err := a.assembleClosedMemberMaterials(t.Context(), bundle, false); err != nil {
		t.Fatalf("retry duplicated original barrier: %v", err)
	}
	original := a.enrollment
	if err := a.prepareJoinedMaterialRuntime(t.Context()); err != nil {
		t.Fatal(err)
	}
	if a.enrollment != original || s.calls.Load() != 1 || a.store != st || a.businessHTTP != gate {
		t.Fatal("material retry duplicated original classification or owners")
	}
}

func assertAssemblyStoreLive(t *testing.T, a *App) {
	t.Helper()
	var one int
	if err := a.store.DB().Raw("SELECT 1").Scan(&one).Error; err != nil || one != 1 {
		t.Fatalf("borrowed Store closed: %d %v", one, err)
	}
}
