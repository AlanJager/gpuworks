//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type cancelingMaterialReader struct {
	cancel context.CancelFunc
	reads  int
}

func (reader *cancelingMaterialReader) Read(p []byte) (int, error) {
	reader.reads++
	reader.cancel()
	return copy(p, "material bytes"), nil
}

func TestMaterialInventoryHashStopsBetweenChunks(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	reader := &cancelingMaterialReader{cancel: cancel}
	if _, err := hashMaterialReader(ctx, reader); !errors.Is(err, context.Canceled) || reader.reads != 1 {
		t.Fatalf("cancelled inventory scan continued reading: reads=%d err=%v", reader.reads, err)
	}
}

func TestInstalledMaterialHashReturnsCancellation(t *testing.T) {
	rootPath := t.TempDir()
	body := []byte(strings.Repeat("x", 1<<20))
	if err := os.WriteFile(filepath.Join(rootPath, "installed"), body, 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := verifyRootMaterial(ctx, root, "installed", materialTestDigest(body), int64(len(body))); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled installed material hash: %v", err)
	}
}

func TestRequiredMaterialInventoryFindsProductReferencesAndMissingBytes(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "gpuworks.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write("master.key", "private master")
	write("cluster-enrollment/ca.key", "private authority")
	offline := write("images/offline.tar", "offline image bytes")
	offlineSHA := materialTestDigest([]byte("offline image bytes"))
	tokenizer := write("benchmark-artifacts/tokenizer-inventory.json", `{"model":{}}`)
	unreferencedTokenizer := write("benchmark-artifacts/tokenizer-123.json", `{"model":{}}`)
	detail := write("benchmark-artifacts/detail.jsonl.gz", "detail bytes")
	write("model-logos/logo.png", "logo bytes")
	write("ssh-hostkeys.json", "trusted host bytes")
	write("dl/gpuworks-agent-linux-amd64", "operator override bytes")
	cache, err := packages.New(filepath.Join(root, "catalog-packages"))
	if err != nil {
		t.Fatal(err)
	}
	archive, err := seed.Archive()
	if err != nil {
		t.Fatal(err)
	}
	digest, _, err := cache.PutArchive(strings.NewReader(string(archive)))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []any{
		&store.InferenceModel{ModelID: "inventory-catalog", PackageDigest: digest},
		&store.Image{ImageRef: "inventory:offline", OfflineBlobPath: "offline.tar", OfflineBlobSHA256: offlineSHA, OfflineBlobBytes: int64(len("offline image bytes"))},
		&store.Image{ImageRef: "inventory:offline-alias", OfflineBlobPath: "offline.tar", OfflineBlobSHA256: offlineSHA, OfflineBlobBytes: int64(len("offline image bytes"))},
		&store.Model{Source: "local", SourceID: "inventory-logo", LogoPath: "logo.png"},
		&store.BenchmarkRun{Name: "inventory", TokenizerRef: tokenizer},
		&store.BenchmarkRun{Name: "remote-tokenizer", TokenizerRef: "https://example.invalid/tokenizer.json"},
		&store.BenchmarkPoint{RunID: 1, Seq: 1, DetailPath: detail},
	} {
		if err := st.DB().Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	inventory, err := InspectRequiredMaterials(t.Context(), root, st)
	if err != nil {
		t.Fatal(err)
	}
	if err := inventory.CheckCoverage(inventory.Classes); err != nil {
		t.Fatal(err)
	}
	if count := len(slices.DeleteFunc(slices.Clone(inventory.Files), func(file MaterialFile) bool { return file.Class != MaterialOfflineImage })); count != 1 {
		t.Fatalf("shared offline archive appeared %d times in the material inventory", count)
	}
	if err := os.WriteFile(offline, []byte("offline image bytex"), 0600); err != nil {
		t.Fatal(err)
	}
	if partial, err := InspectRequiredMaterials(t.Context(), root, st); err == nil || len(partial.Files) != 0 {
		t.Fatalf("catalog-mismatched offline bytes published: %+v %v", partial, err)
	}
	if err := os.WriteFile(offline, []byte("offline image bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(inventory.Files, func(file MaterialFile) bool {
		return file.Class == MaterialBenchmarkInput && file.Path == unreferencedTokenizer
	}) {
		t.Fatal("accepted tokenizer upload disappeared before a run referenced it")
	}
	for _, class := range requiredMaterialClasses {
		if !slices.ContainsFunc(inventory.Files, func(file MaterialFile) bool { return file.Class == class && len(file.SHA256) == 64 }) {
			t.Fatalf("%s source bytes missing from inventory", class)
		}
		covered := slices.DeleteFunc(slices.Clone(inventory.Classes), func(candidate MaterialClass) bool { return candidate == class })
		if err := inventory.CheckCoverage(covered); err == nil || !strings.Contains(err.Error(), string(class)) {
			t.Fatalf("omitted %s class accepted: %v", class, err)
		}
	}
	local, err := persistLocalMaterialInventory(t.Context(), root, st, 7, inventory)
	if err != nil {
		t.Fatalf("ten-class local material persistence failed: %v", err)
	}
	for _, file := range inventory.Files {
		path := local[file.SHA256]
		if path != filepath.Join(root, "material-replicas", file.SHA256[:2], file.SHA256) {
			t.Fatalf("%s lacked a canonical local path", file.Class)
		}
		if digest, err := hashFile(path); err != nil || digest != file.SHA256 {
			t.Fatalf("%s canonical local bytes differ: %v", file.Class, err)
		}
	}
	identity := store.ClusterIdentity{ClusterID: strings.Repeat("a", 32)}
	key := strings.Repeat("b", 64)
	rawManifest, err := buildMaterialManifest(root, inventory, local, identity, 7, key)
	if err != nil {
		t.Fatalf("ten-class consumer-path manifest failed: %v", err)
	}
	manifest, err := parseMaterialManifest(rawManifest, identity, 7, key)
	if err != nil || len(manifest.Files) != len(inventory.Files) {
		t.Fatalf("ten-class manifest omitted paths: %d/%d %v", len(manifest.Files), len(inventory.Files), err)
	}
	if err := st.DB().Model(&store.Image{}).Where("image_ref = ?", "inventory:offline-alias").Update("offline_blob_sha256", strings.Repeat("0", 64)).Error; err != nil {
		t.Fatal(err)
	}
	if err := checkMaterialConsumerReferences(t.Context(), st, &materialReceiver{root: filepath.Join(root, "material-replicas"), memberID: 8}, manifest); err == nil {
		t.Fatal("receiver accepted a manifest that disagrees with a current offline image row")
	}
	if err := st.DB().Model(&store.Image{}).Where("image_ref = ?", "inventory:offline-alias").Update("offline_blob_sha256", offlineSHA).Error; err != nil {
		t.Fatal(err)
	}
	if err := checkMaterialConsumerReferences(t.Context(), st, &materialReceiver{root: filepath.Join(root, "material-replicas"), memberID: 8}, manifest); err != nil {
		t.Fatalf("local benchmark references present in the manifest were rejected: %v", err)
	}
	if err := checkMaterialConsumerReferences(t.Context(), st, &materialReceiver{root: filepath.Join(t.TempDir(), "material-replicas"), memberID: 8}, manifest); err == nil {
		t.Fatal("target member accepted benchmark references bound to the source data directory")
	}
	if err := st.DB().Model(&store.BenchmarkRun{}).Where("tokenizer_ref = ?", tokenizer).Update("tokenizer_ref", "artifact://"+filepath.Base(tokenizer)).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.BenchmarkPoint{}).Where("detail_path = ?", detail).Update("detail_path", filepath.Base(detail)).Error; err != nil {
		t.Fatal(err)
	}
	for i, file := range inventory.Files {
		rel, err := filepath.Rel(root, file.Path)
		if err != nil || manifest.Files[i].Class != file.Class || manifest.Files[i].Path != filepath.ToSlash(rel) || manifest.Files[i].SHA256 != file.SHA256 {
			t.Fatalf("%s consumer mapping differs: %+v %v", file.Class, manifest.Files[i], err)
		}
	}
	target := t.TempDir()
	if err := os.Mkdir(filepath.Join(target, "material-replicas"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "master.key"), []byte("private master"), 0600); err != nil {
		t.Fatal(err)
	}
	targetIdentity := filepath.Join(target, "cluster-enrollment", "ca.key")
	if err := os.MkdirAll(filepath.Dir(targetIdentity), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetIdentity, []byte("target's own authority"), 0600); err != nil {
		t.Fatal(err)
	}
	receiver, err := newMaterialReceiver(filepath.Join(target, "material-replicas"), 8)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkMaterialConsumerReferences(t.Context(), st, receiver, manifest); err != nil {
		t.Fatalf("target member rejected portable benchmark references: %v", err)
	}
	for digest, source := range local {
		file, err := os.Open(source)
		if err != nil {
			t.Fatal(err)
		}
		_, receivedErr := receiver.receive(t.Context(), digest, file)
		closeErr := file.Close()
		if err := errors.Join(receivedErr, closeErr); err != nil {
			t.Fatal(err)
		}
	}
	if err := checkInstalledMaterialManifest(t.Context(), receiver, manifest); err == nil {
		t.Fatal("uninstalled consumer paths were reported as installed")
	}
	logoDir := filepath.Join(target, "model-logos")
	if err := os.Mkdir(logoDir, 0700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(logoDir, ".material-"+strings.Repeat("a", 32))
	if err := os.WriteFile(orphan, []byte("interrupted logo installation"), 0600); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(logoDir, ".material-keep")
	if err := os.WriteFile(unrelated, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	packageDir := filepath.Join(target, "catalog-packages")
	packageStage := filepath.Join(packageDir, ".material-package-123456789")
	if err := os.MkdirAll(packageStage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageStage, "partial"), []byte("interrupted package installation"), 0600); err != nil {
		t.Fatal(err)
	}
	unrelatedPackageDir := filepath.Join(packageDir, ".material-package-keep")
	if err := os.Mkdir(unrelatedPackageDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := materializeReceivedManifest(t.Context(), receiver, manifest); err != nil {
		t.Fatalf("receiver could not install ten-class manifest: %v", err)
	}
	if err := checkInstalledMaterialManifest(t.Context(), receiver, manifest); err != nil {
		t.Fatalf("installed consumer paths were not readable: %v", err)
	}
	wrongCatalog := manifest
	wrongCatalog.Files = append([]materialManifestFile(nil), manifest.Files...)
	for i := range wrongCatalog.Files {
		if wrongCatalog.Files[i].Class == MaterialCatalogPackage {
			wrongCatalog.Files[i].SHA256 = materialTestDigest([]byte("different package file"))
			break
		}
	}
	if err := checkInstalledMaterialManifest(t.Context(), receiver, wrongCatalog); err == nil {
		t.Fatal("installed package digest hid a mismatched manifest file")
	}
	if _, err := os.Lstat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("material retry retained interrupted consumer staging: %v", err)
	}
	if got, err := os.ReadFile(unrelated); err != nil || string(got) != "keep" {
		t.Fatalf("material retry changed unrelated consumer file: %q %v", got, err)
	}
	if _, err := os.Lstat(packageStage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("material retry retained interrupted package staging: %v", err)
	}
	if info, err := os.Lstat(unrelatedPackageDir); err != nil || !info.IsDir() {
		t.Fatalf("material retry changed unrelated package directory: %v %v", info, err)
	}
	if err := materializeReceivedManifest(t.Context(), receiver, manifest); err != nil {
		t.Fatalf("identical material retry was not idempotent: %v", err)
	}
	for _, file := range manifest.Files {
		if file.Class == MaterialMemberIdentity {
			if file.Path == "cluster-enrollment/ca.key" {
				if retained, err := os.ReadFile(targetIdentity); err != nil || string(retained) != "target's own authority" {
					t.Fatalf("target enrollment identity was replaced: %q %v", retained, err)
				}
				continue
			}
			if _, err := os.Lstat(filepath.Join(target, filepath.FromSlash(file.Path))); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("source member identity was installed at %s: %v", file.Path, err)
			}
			continue
		}
		installed := filepath.Join(target, filepath.FromSlash(file.Path))
		actual, err := os.ReadFile(installed)
		if err != nil || len(actual) == 0 {
			t.Fatalf("%s material was not readable at %s: %v", file.Class, file.Path, err)
		}
		if got, err := hashFile(installed); err != nil || got != file.SHA256 {
			t.Fatalf("%s installed bytes differ at %s: %v", file.Class, file.Path, err)
		}
	}
	targetCatalog, err := packages.New(filepath.Join(target, "catalog-packages"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := targetCatalog.Load(digest); err != nil {
		t.Fatalf("installed catalog package cannot be consumed: %v", err)
	}
	assetRead := false
	for _, file := range manifest.Files {
		if file.Class != MaterialRuntimeAsset {
			continue
		}
		modelPath := strings.TrimPrefix(file.Path, "catalog-packages/"+digest+"/models/")
		separator := strings.Index(modelPath, "/assets/")
		if separator < 1 {
			t.Fatalf("runtime asset path is not inside a model: %s", file.Path)
		}
		body, err := targetCatalog.ReadAsset(digest, modelPath[:separator], modelPath[separator+1:])
		if err != nil || len(body) == 0 {
			t.Fatalf("installed runtime asset cannot be read by its consumer: %v", err)
		}
		assetRead = true
		break
	}
	if !assetRead {
		t.Fatal("fixture did not exercise a runtime asset consumer")
	}
	conflict := filepath.Join(target, "model-logos", "logo.png")
	if err := os.WriteFile(conflict, []byte("different logo"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkInstalledMaterialManifest(t.Context(), receiver, manifest); err == nil {
		t.Fatal("damaged consumer path was reported as installed")
	}
	if err := materializeReceivedManifest(t.Context(), receiver, manifest); err == nil {
		t.Fatal("conflicting consumer file was overwritten or accepted")
	}
	if actual, err := os.ReadFile(conflict); err != nil || string(actual) != "different logo" {
		t.Fatalf("conflicting consumer file was replaced: %q %v", actual, err)
	}
	if err := os.Remove(detail); err != nil {
		t.Fatal(err)
	}
	if partial, err := InspectRequiredMaterials(t.Context(), root, st); err == nil || len(partial.Files) != 0 {
		t.Fatalf("missing committed detail returned a usable partial inventory: %+v %v", partial, err)
	}
}

func TestRequiredMaterialInventoryRejectsEscapingReferencesAndMissingIdentity(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "gpuworks.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := os.WriteFile(filepath.Join(root, "master.key"), []byte("master"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.Image{ImageRef: "inventory:escape", OfflineBlobPath: "../outside.tar"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := InspectRequiredMaterials(t.Context(), root, st); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("escaped image reference accepted: %v", err)
	}
	if err := st.DB().Model(&store.Image{}).Where("image_ref = ?", "inventory:escape").Update("offline_blob_path", "").Error; err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.tar")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "images"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "images", "linked.tar")); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Image{}).Where("image_ref = ?", "inventory:escape").Update("offline_blob_path", "linked.tar").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := InspectRequiredMaterials(t.Context(), root, st); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("linked image reference accepted: %v", err)
	}
	if err := st.DB().Model(&store.Image{}).Where("image_ref = ?", "inventory:escape").Update("offline_blob_path", "").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := InspectRequiredMaterials(t.Context(), root, st); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "master.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectRequiredMaterials(t.Context(), root, st); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing master secret accepted: %v", err)
	}
}
