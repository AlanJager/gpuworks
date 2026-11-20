//go:build linux && dqlite

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func inventoryIdentityStagingFixture(t *testing.T) (string, *store.Store, string, identity.TransportPolicy) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "gpuworks.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := os.WriteFile(filepath.Join(root, "master.key"), []byte("inventory fixture master"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "gpuworks.db.dqlite", ".transport")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key, err := identity.CreateMemberKey(filepath.Join(dir, "member.key"))
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
	policy := identity.TransportPolicy{ClusterID: strings.Repeat("a", 32), CAFingerprint: strings.Repeat("b", 64), Revision: 1, SourceRevision: 1, AllowedKeys: []string{fingerprint}}
	path := filepath.Join(dir, "policy.json")
	if err := identity.PublishTransportPolicy(path, 0, policy); err != nil {
		t.Fatal(err)
	}
	return root, st, path, policy
}

func TestMaterialInventoryIdentityExcludesOnlyTransportWriterStaging(t *testing.T) {
	root, st, policyPath, _ := inventoryIdentityStagingFixture(t)
	// Reproduce the writer's real CreateTemp prefix and partially written bytes.
	staging, err := os.CreateTemp(filepath.Dir(policyPath), ".transport-policy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(staging.Name()) })
	if _, err := staging.WriteString(`{"revision":`); err != nil {
		_ = staging.Close()
		t.Fatal(err)
	}
	if err := staging.Close(); err != nil {
		t.Fatal(err)
	}
	kept := []string{
		policyPath,
		filepath.Join(root, "cluster-enrollment", ".transport-policy-final"),
		filepath.Join(root, "cluster-enrollment", ".transport", ".transport-policy-final"),
		filepath.Join(root, "dl", ".transport-policy-final"),
	}
	for _, path := range kept[1:] {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("published bytes at "+filepath.Base(filepath.Dir(path))), 0600); err != nil {
			t.Fatal(err)
		}
	}
	inventory, err := InspectRequiredMaterials(t.Context(), root, st)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, file := range inventory.Files {
		paths[file.Path] = true
	}
	if paths[staging.Name()] {
		t.Error("unpublished transport writer staging bytes entered the inventory")
	}
	for _, path := range kept {
		if !paths[path] {
			t.Errorf("published policy or similarly named material was omitted: %s", path)
		}
	}
}

func TestMaterialInventoryIdentityReplacementStillRejectsStaleSnapshot(t *testing.T) {
	root, st, path, policy := inventoryIdentityStagingFixture(t)
	inventory, err := InspectRequiredMaterials(t.Context(), root, st)
	if err != nil {
		t.Fatal(err)
	}
	var oldDigest string
	for _, file := range inventory.Files {
		if file.Path == path {
			oldDigest = file.SHA256
		}
	}
	if oldDigest == "" {
		t.Fatal("published transport policy absent from original inventory")
	}
	// The actual writer publishes a new inode after the original scan.
	policy.Revision++
	policy.SourceRevision++
	if err := identity.PublishTransportPolicy(path, 1, policy); err != nil {
		t.Fatal(err)
	}
	local, err := persistLocalMaterialInventory(t.Context(), root, st, 7, inventory)
	if err == nil || !strings.Contains(err.Error(), "material transfer digest mismatch") || local != nil {
		t.Fatalf("stale identity snapshot was accepted: local=%v err=%v", local, err)
	}
	for _, model := range []any{&store.MaterialReplica{}, &store.MaterialManifestRecord{}} {
		var count int64
		if err := st.DB().Model(model).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("failed snapshot published indexed state: %T count=%d err=%v", model, count, err)
		}
	}
	fresh, err := InspectRequiredMaterials(t.Context(), root, st)
	if err != nil {
		t.Fatal(err)
	}
	local, err = persistLocalMaterialInventory(t.Context(), root, st, 7, fresh)
	if err != nil {
		t.Fatalf("fresh snapshot failed after legitimate policy replacement: %v", err)
	}
	if _, exists := local[oldDigest]; exists {
		t.Fatal("fresh snapshot reused the stale policy digest")
	}
	for _, file := range fresh.Files {
		if digest, err := hashFile(local[file.SHA256]); err != nil || digest != file.SHA256 {
			t.Fatalf("fresh immutable copy differs: digest=%s err=%v", digest, err)
		}
	}
	var oldReferences int64
	if err := st.DB().Model(&store.MaterialReplica{}).Where("digest = ?", oldDigest).Count(&oldReferences).Error; err != nil || oldReferences != 0 {
		t.Fatalf("stale policy gained a reference: count=%d err=%v", oldReferences, err)
	}
}
