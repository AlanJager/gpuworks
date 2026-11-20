//go:build linux && dqlite

package app

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestOrdinaryEnrollmentRecoversMaterialStagesBeforeListenerStarts(t *testing.T) {
	ctx := t.Context()
	clusterID := strings.Repeat("a", 32)
	now := time.Now().UTC()
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
	config, err := enrollmentClientTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}, ca)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(dataDir, "gpuworks.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.InitializeClusterIdentity(ctx, store.ClusterIdentity{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	st.NoteClusterSessionRoute(store.ClusterSessionRoute{ID: 7, Key: []byte("local-key"), Listen: "127.0.0.1:9910"})
	signer, err := auth.NewTokenSigner("startup-material-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bundle := &enrollmentBundle{info: EnrollmentInfo{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion, ListenAddress: "127.0.0.1:0", PublicURL: "https://127.0.0.1:9910"}, issuer: authority, tls: config, trust: trust, dataDir: dataDir}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := filepath.Join(dataDir, "material-replicas")
	outside := t.TempDir()
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	if server, err := prepareEnrollmentServer(bundle, st, signer, log, nil); err == nil || server != nil {
		t.Fatalf("symlinked receiver root opened an enrollment listener: server=%v err=%v", server, err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "ab")
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(parent, ".receiving-abandoned")
	if err := os.WriteFile(orphan, []byte("interrupted upload"), 0600); err != nil {
		t.Fatal(err)
	}
	published := filepath.Join(parent, strings.Repeat("b", 64))
	if err := os.WriteFile(published, []byte("stable replica"), 0600); err != nil {
		t.Fatal(err)
	}
	server, err := prepareEnrollmentServer(bundle, st, signer, log, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Fatalf("listener was prepared before orphan cleanup: %v", err)
	}
	if got, err := os.ReadFile(published); err != nil || string(got) != "stable replica" {
		t.Fatalf("startup cleanup changed published replica: %q %v", got, err)
	}
	if server.materialRequests == nil {
		t.Fatal("prepared material receiver has no lifecycle owner")
	}
	// Preparing routes for the already-open closed listener must borrow its
	// receiver and work tracker. Re-running startup recovery while that listener
	// accepts material writes could remove an in-flight temporary file.
	secondOrphan := filepath.Join(parent, ".receiving-inflight")
	if err := os.WriteFile(secondOrphan, []byte("active transfer"), 0600); err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareEnrollmentServerWithOwner(bundle, st, signer, log, nil, server)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.materialReceiver != server.materialReceiver || prepared.materialRequests != server.materialRequests {
		t.Fatal("prepared routes replaced the closed listener's material owner")
	}
	if got, err := os.ReadFile(secondOrphan); err != nil || string(got) != "active transfer" {
		t.Fatalf("preparation ran orphan recovery on the live receiver: %q %v", got, err)
	}
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	address := server.Addr()
	if err := server.adoptPreparedHandler(prepared); err != nil {
		t.Fatalf("closed listener rejected its own prepared material owner: %v", err)
	}
	if server.Addr() != address || prepared.run != nil {
		t.Fatal("prepared material routes started or replaced the closed listener")
	}
	wrongOwner := &enrollmentServer{
		materialReceiver: &materialReceiver{root: server.materialReceiver.root, memberID: server.materialReceiver.memberID + 1},
		materialRequests: server.materialRequests,
	}
	if candidate, err := prepareEnrollmentServerWithOwner(bundle, st, signer, log, nil, wrongOwner); err == nil || candidate != nil {
		t.Fatalf("accepted material owner for another member: server=%v err=%v", candidate, err)
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := server.Close(shutdown); err != nil {
		t.Fatal(err)
	}
}
