//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A user package and an older package travel over the real enrollment catalog
// listener. The older package is referenced only by a deployment snapshot, and
// the receiver reads the asset bytes with the same call a deployment uses.
func TestEnrollmentCatalogTransfersUserAndOldPackagesForDeploymentRead(t *testing.T) {
	member := startCatalogTransferMember(t)
	currentAsset := []byte("print('current user asset')\n")
	oldAsset := []byte("print('retired deployment asset')\n")
	unusedAsset := []byte("print('unreferenced package')\n")
	currentFS := userCatalogPackage(t, "org/current-model", currentAsset)
	oldFS := userCatalogPackage(t, "org/retired-model", oldAsset)
	unusedFS := userCatalogPackage(t, "org/unreferenced-model", unusedAsset)
	currentDigest, _, err := member.catalog.Put(currentFS)
	if err != nil {
		t.Fatal(err)
	}
	oldDigest, _, err := member.catalog.Put(oldFS)
	if err != nil {
		t.Fatal(err)
	}
	unusedDigest, _, err := member.catalog.Put(unusedFS)
	if err != nil {
		t.Fatal(err)
	}
	if currentDigest == oldDigest || currentDigest == unusedDigest || oldDigest == unusedDigest {
		t.Fatalf("package identities collided: %s %s %s", currentDigest, oldDigest, unusedDigest)
	}

	index, err := member.client.FetchCatalogIndex(t.Context(), member.cert, member.key)
	if err != nil || len(index.Digests) != 0 {
		t.Fatalf("disk packages without references: %+v %v", index, err)
	}
	for _, digest := range []string{currentDigest, oldDigest, unusedDigest} {
		member.rejectPackage(t, digest, "HTTP 404")
	}

	if err := member.store.DB().Create(&store.InferenceModel{ModelID: "org/current-model", Source: "user", PackageDigest: currentDigest}).Error; err != nil {
		t.Fatal(err)
	}
	index, err = member.client.FetchCatalogIndex(t.Context(), member.cert, member.key)
	if err != nil || !slices.Equal(index.Digests, []string{currentDigest}) {
		t.Fatalf("user package index: %+v %v", index, err)
	}
	member.rejectPackage(t, oldDigest, "HTTP 404")
	member.rejectPackage(t, unusedDigest, "HTTP 404")
	receiver := member.receive(t, currentDigest)
	if got, err := receiver.ReadAsset(currentDigest, "org/current-model", "assets/serve.py"); err != nil || !bytes.Equal(got, currentAsset) {
		t.Fatalf("receiver user asset=%q %v", got, err)
	}

	oldSHA := sha256.Sum256(oldAsset)
	reference := proto.RuntimeAsset{
		File: "assets/serve.py", SHA256: hex.EncodeToString(oldSHA[:]), SizeBytes: int64(len(oldAsset)),
		UnpackTo: "/opt/gpuworks/serve.py", PackageDigest: oldDigest, ModelID: "org/retired-model",
	}
	snapshot, err := json.Marshal(map[string]any{"runtime_assets": []proto.RuntimeAsset{reference}})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(snapshot, oldAsset) || bytes.Contains(snapshot, []byte(`"content"`)) {
		t.Fatalf("deployment snapshot embedded asset bytes: %s", snapshot)
	}
	if err := member.store.DB().Create(&store.Deployment{Name: "retired-launch", CatalogSelectionSnapshot: string(snapshot)}).Error; err != nil {
		t.Fatal(err)
	}
	var models []store.InferenceModel
	if err := member.store.DB().Find(&models).Error; err != nil || len(models) != 1 || models[0].ModelID != "org/current-model" || models[0].PackageDigest != currentDigest {
		t.Fatalf("old package gained a current model row: %+v %v", models, err)
	}
	index, err = member.client.FetchCatalogIndex(t.Context(), member.cert, member.key)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{currentDigest, oldDigest}
	sort.Strings(want)
	if !slices.Equal(index.Digests, want) || slices.Contains(index.Digests, unusedDigest) {
		t.Fatalf("referenced index=%v", index.Digests)
	}
	if got, created, err := receiver.Put(member.fetch(t, oldDigest).FS); err != nil || !created || got != oldDigest {
		t.Fatal("persist old package", got, created, err)
	}
	got, err := receiver.ReadAsset(oldDigest, reference.ModelID, reference.File)
	sum := sha256.Sum256(got)
	if err != nil || !bytes.Equal(got, oldAsset) || hex.EncodeToString(sum[:]) != reference.SHA256 {
		t.Fatalf("deployment asset read=%q %v", got, err)
	}
	member.rejectPackage(t, unusedDigest, "HTTP 404")

	for _, digest := range []string{currentDigest, oldDigest} {
		if err := os.RemoveAll(filepath.Join(member.root, digest)); err != nil {
			t.Fatal(err)
		}
		member.rejectPackage(t, digest, "HTTP 503")
	}
	if got, err := receiver.ReadAsset(currentDigest, "org/current-model", "assets/serve.py"); err != nil || !bytes.Equal(got, currentAsset) {
		t.Fatalf("receiver lost user asset after source removal: %q %v", got, err)
	}
	if got, err := receiver.ReadAsset(oldDigest, reference.ModelID, reference.File); err != nil || !bytes.Equal(got, oldAsset) {
		t.Fatalf("receiver lost deployment asset after source removal: %q %v", got, err)
	}
	if _, err := receiver.ReadAsset(unusedDigest, "org/unreferenced-model", "assets/serve.py"); err == nil {
		t.Fatal("unreferenced package was copied to the receiver")
	}
}

type catalogTransferMember struct {
	store   *store.Store
	catalog *packages.Store
	root    string
	client  *enrollment.Client
	cert    []byte
	key     []byte
}

func (m catalogTransferMember) fetch(t *testing.T, digest string) inferencecatalog.Archive {
	t.Helper()
	archive, err := m.client.FetchCatalogPackage(t.Context(), m.cert, m.key, digest)
	if err != nil {
		t.Fatal(err)
	}
	return archive
}

func (m catalogTransferMember) receive(t *testing.T, digest string) *packages.Store {
	t.Helper()
	receiver, err := packages.New(filepath.Join(t.TempDir(), "receiver"))
	if err != nil {
		t.Fatal(err)
	}
	got, created, err := receiver.Put(m.fetch(t, digest).FS)
	if err != nil || !created || got != digest {
		t.Fatal("persist received package", got, created, err)
	}
	return receiver
}

func (m catalogTransferMember) rejectPackage(t *testing.T, digest, status string) {
	t.Helper()
	archive, err := m.client.FetchCatalogPackage(t.Context(), m.cert, m.key, digest)
	if err == nil || archive.FS != nil || !strings.Contains(err.Error(), status) {
		t.Fatalf("package %s status=%v archive=%v", digest, err, archive.FS != nil)
	}
}

func startCatalogTransferMember(t *testing.T) catalogTransferMember {
	t.Helper()
	now := time.Now()
	clusterID := strings.Repeat("c", 32)
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
	issue := func(host string) (tls.Certificate, string, []byte) {
		t.Helper()
		key, err := identity.NewMemberKey()
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
		cert, err := authority.IssueMember(csr, fp, host, now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		pair, err := tls.X509KeyPair(append(cert, ca...), key)
		if err != nil {
			t.Fatal(err)
		}
		return pair, fp, key
	}
	serverPair, serverFP, _ := issue("127.0.0.1:9920")
	clientPair, clientFP, clientKey := issue("127.0.0.1:9921")
	config, err := enrollmentClientTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverPair}}, ca)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "catalog-transfer.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.InitializeClusterIdentity(t.Context(), store.ClusterIdentity{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InitializeClusterSeedAdmission(t.Context(), clusterID, serverFP, "127.0.0.1:9920"); err != nil {
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
	if _, err := st.ClaimClusterInvitation(t.Context(), clusterID, token, clientFP, "127.0.0.1:9921"); err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("catalog-transfer-member", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "source-catalog")
	catalog, err := packages.New(root)
	if err != nil {
		t.Fatal(err)
	}
	bundle := &enrollmentBundle{
		info:   EnrollmentInfo{ClusterID: clusterID, CAFingerprint: pin, Protocol: store.ClusterProtocolVersion, ListenAddress: "127.0.0.1:0", PublicURL: "https://127.0.0.1:9920"},
		issuer: authority, tls: config, master: "catalog-transfer-member", trust: trust, ca: ca, caKey: caKey,
	}
	server, err := prepareEnrollmentServer(bundle, st, signer, slog.New(slog.NewTextHandler(io.Discard, nil)), catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Close(ctx)
	})
	client, err := enrollment.NewClient("https://"+server.Addr()+"/api/v1/cluster/enrollment/certificate", clusterID, pin, store.ClusterProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return catalogTransferMember{
		store: st, catalog: catalog, root: root, client: client,
		cert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientPair.Certificate[0]}),
		key:  clientKey,
	}
}

func userCatalogPackage(t *testing.T, modelID string, asset []byte) fs.FS {
	t.Helper()
	sum := sha256.Sum256(asset)
	model := inferencecatalog.InferenceModel{
		ModelID: modelID, Source: "user", DisplayTitle: modelID,
		Specs: []inferencecatalog.InferenceSpec{{
			Name: "default", Engine: "vllm", Image: "vllm/vllm-openai:latest",
			Weights: inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: modelID}}},
			GPUs:    inferencecatalog.GPUSelector{Vendor: "nvidia", CC: ">=8.0"},
			Assets:  []inferencecatalog.Asset{{File: "assets/serve.py", SHA256: hex.EncodeToString(sum[:]), UnpackTo: "/opt/gpuworks/serve.py"}},
		}},
	}
	fsys, err := inferencecatalog.BuildPackageFS("2026-09-21T00:00:00Z", inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{model}}, map[string][]byte{
		"models/" + modelID + "/assets/serve.py": asset,
		"models/" + modelID + "/README.en.md":    []byte("user package " + modelID),
	})
	if err != nil {
		t.Fatal(err)
	}
	return fsys
}
