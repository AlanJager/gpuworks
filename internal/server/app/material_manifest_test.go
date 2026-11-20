//go:build linux && dqlite

package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gorm.io/gorm"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestReceivedMaterialIndexBatchesAndRejectsMissingOrRedirectedRows(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "materials.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	replicaRoot := filepath.Join(root, "replicas")
	if err := os.Mkdir(replicaRoot, 0700); err != nil {
		t.Fatal(err)
	}
	receiver, err := newMaterialReceiver(replicaRoot, 7)
	if err != nil {
		t.Fatal(err)
	}
	const files = 130 // Cross two 64-row query boundaries.
	digests := make([]string, files)
	rows := make([]store.MaterialReplica, files)
	for i := range digests {
		digests[i] = materialTestDigest([]byte(fmt.Sprintf("material-%d", i)))
		rows[i] = store.MaterialReplica{
			Digest: digests[i], MemberID: int64(receiver.memberID),
			Path: filepath.Join(replicaRoot, digests[i][:2], digests[i]),
		}
	}
	if err := st.DB().Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	queries := 0
	if err := st.DB().Callback().Query().Before("gorm:query").Register("test:count_material_index_queries", func(tx *gorm.DB) {
		if tx.Statement.Table == "material_replicas" {
			queries++
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := checkReceivedMaterialRows(t.Context(), st, receiver, digests); err != nil {
		t.Fatal(err)
	}
	if queries != 3 {
		t.Fatalf("material index lookup used %d queries for %d digests, want 3", queries, files)
	}
	if err := st.DB().Where("digest = ? AND member_id = ?", digests[files-1], int64(receiver.memberID)).Delete(&store.MaterialReplica{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := checkReceivedMaterialRows(t.Context(), st, receiver, digests); err == nil {
		t.Fatal("missing row at the last batch boundary was accepted")
	}
	if err := st.DB().Create(&rows[files-1]).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.MaterialReplica{}).Where("digest = ? AND member_id = ?", digests[64], int64(receiver.memberID)).Update("path", filepath.Join(root, "redirected")).Error; err != nil {
		t.Fatal(err)
	}
	if err := checkReceivedMaterialRows(t.Context(), st, receiver, digests); err == nil {
		t.Fatal("redirected row at the middle batch boundary was accepted")
	}
}

func TestMaterialManifestPinsConsumerPathCoverageAndSource(t *testing.T) {
	root := t.TempDir()
	body := []byte("private master bytes")
	path := filepath.Join(root, "master.key")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	digest, err := hashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	identity := store.ClusterIdentity{ClusterID: strings.Repeat("a", 32)}
	key := strings.Repeat("b", 64)
	inventory := MaterialInventory{Classes: slices.Clone(requiredMaterialClasses), Files: []MaterialFile{{Class: MaterialMasterSecret, Path: path, SHA256: digest}}}
	raw, err := buildMaterialManifest(root, inventory, map[string]string{digest: path}, identity, 7, key)
	if err != nil {
		t.Fatal(err)
	}
	var manifest materialManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Classes) != len(requiredMaterialClasses) || len(manifest.Files) != 1 || manifest.Files[0].Path != "master.key" || manifest.Files[0].Bytes != int64(len(body)) {
		t.Fatalf("manifest lost a required class or consumer path: %+v", manifest)
	}
	if _, err := parseMaterialManifest(raw, identity, 7, key); err != nil {
		t.Fatal(err)
	}
	for _, malformed := range [][]byte{
		append(slices.Clone(raw), []byte(" ")...),
		[]byte(strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1)),
		[]byte(strings.Replace(string(raw), `"version":1`, `"unknown":1,"version":1`, 1)),
	} {
		if _, err := parseMaterialManifest(malformed, identity, 7, key); err == nil {
			t.Fatalf("noncanonical manifest was accepted: %s", malformed)
		}
	}
	for _, mutate := range []func(*materialManifest){
		func(m *materialManifest) { m.Classes = m.Classes[:len(m.Classes)-1] },
		func(m *materialManifest) { m.SourceMemberID++ },
		func(m *materialManifest) { m.SourceKey = strings.Repeat("c", 64) },
		func(m *materialManifest) { m.Files[0].Path = "../master.key" },
		func(m *materialManifest) { m.Files[0].Path = "images/../master.key" },
		func(m *materialManifest) { m.Files[0].Path = "dl/agent" },
		func(m *materialManifest) { m.Files[0].Bytes = 0 },
		func(m *materialManifest) { m.Files[0].SHA256 = strings.Repeat("f", 63) },
		func(m *materialManifest) { m.Files = append(m.Files, m.Files[0]) },
		func(m *materialManifest) {
			m.Files = append(m.Files, materialManifestFile{Class: MaterialOfflineImage, Path: "images/cached.tar", SHA256: m.Files[0].SHA256, Bytes: m.Files[0].Bytes + 1})
		},
	} {
		var changed materialManifest
		if err := json.Unmarshal(raw, &changed); err != nil {
			t.Fatal(err)
		}
		mutate(&changed)
		if err := validateMaterialManifest(changed, identity, 7, key); err == nil {
			t.Fatalf("invalid manifest passed validation: %+v", changed)
		}
	}
	outside := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(outside, body, 0600); err != nil {
		t.Fatal(err)
	}
	inventory.Files[0].Path = outside
	if _, err := buildMaterialManifest(root, inventory, map[string]string{digest: path}, identity, 7, key); err == nil {
		t.Fatal("source path outside data root was accepted")
	}
}
