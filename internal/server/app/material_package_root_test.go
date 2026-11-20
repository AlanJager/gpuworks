//go:build linux && dqlite

package app

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
)

func receivedPackageRootFixture(t *testing.T) (string, *materialReceiver, materialManifest, map[string][]byte, string) {
	t.Helper()
	dataDir := t.TempDir()
	replicaDir := filepath.Join(dataDir, "material-replicas")
	if err := os.Mkdir(replicaDir, 0700); err != nil {
		t.Fatal(err)
	}
	receiver, err := newMaterialReceiver(replicaDir, 2)
	if err != nil {
		t.Fatal(err)
	}
	model := inferencecatalog.InferenceModel{ModelID: "org/root-model", DisplayTitle: "Root Model", Specs: []inferencecatalog.InferenceSpec{{Name: "default", Engine: "vllm", Image: "vllm/vllm-openai:latest", Weights: inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: "org/root-model"}}}, GPUs: inferencecatalog.GPUSelector{Vendor: "nvidia", CC: ">=8.0"}}}}
	yaml, err := inferencecatalog.MarshalModelYAML(model)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"manifest.yaml": []byte("schema_version: 1\npublished_at: 2026-10-06T00:00:00Z\nmodels: 1\nspecs: 1\n"), "models/org/root-model/model.yaml": yaml, "models/org/root-model/README.en.md": []byte("root-bound consumer documentation")}
	digest, err := inferencecatalog.PackageDigest(fsFixture(files))
	if err != nil {
		t.Fatal(err)
	}
	manifest := materialManifest{SourceMemberID: 1}
	for name, body := range files {
		sha := materialTestDigest(body)
		if _, err := receiver.receive(t.Context(), sha, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		manifest.Files = append(manifest.Files, materialManifestFile{Class: MaterialCatalogPackage, Path: "catalog-packages/" + digest + "/" + name, SHA256: sha, Bytes: int64(len(body))})
	}
	return dataDir, receiver, manifest, files, digest
}

func TestReceivedPackageKeepsCapturedRootAfterDataDirectoryReplacement(t *testing.T) {
	dataDir, receiver, manifest, _, digest := receivedPackageRootFixture(t)
	source, err := os.OpenRoot(receiver.root)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := os.OpenRoot(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	original := dataDir + "-original"
	if err := os.Rename(dataDir, original); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(original)
	if err := os.Mkdir(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dataDir, "foreign-owner")
	if err := os.WriteFile(sentinel, []byte("unrelated root"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	err = installReceivedPackage(t.Context(), source, target, dataDir, digest, manifest.Files)
	if err == nil {
		t.Error("replaced consumer root received a false installation acknowledgment")
	}
	if _, err := os.Lstat(filepath.Join(dataDir, "catalog-packages")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("installer entered foreign root: %v", err)
	}
	after, err := os.Stat(sentinel)
	if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("foreign sentinel changed: %v", err)
	}
	body, err := os.ReadFile(sentinel)
	if err != nil || string(body) != "unrelated root" {
		t.Fatalf("foreign bytes changed: %q %v", body, err)
	}
	t.Log("captured consumer root replacement must reject acknowledgment without entering the foreign root")
}

func TestReceivedPackageRootInstallAndReadOnlyProof(t *testing.T) {
	_, receiver, manifest, files, digest := receivedPackageRootFixture(t)
	if err := materializeReceivedManifest(t.Context(), receiver, manifest); err != nil {
		t.Fatal(err)
	}
	if err := checkInstalledMaterialManifest(t.Context(), receiver, manifest); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(filepath.Dir(receiver.root))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	before := make(map[string]fs.FileInfo)
	for name := range files {
		full := "catalog-packages/" + digest + "/" + name
		before[name], err = root.Lstat(full)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := materializeReceivedManifest(t.Context(), receiver, manifest); err != nil {
		t.Fatal(err)
	}
	if err := checkInstalledMaterialManifest(t.Context(), receiver, manifest); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		full := "catalog-packages/" + digest + "/" + name
		after, err := root.Lstat(full)
		if err != nil || !os.SameFile(before[name], after) || !before[name].ModTime().Equal(after.ModTime()) {
			t.Fatalf("no-op changed %s: %v", name, err)
		}
		data, err := root.ReadFile(full)
		if err != nil || !bytes.Equal(data, body) {
			t.Fatalf("consumer %s differs: %v", name, err)
		}
	}
	cache, err := packages.New(filepath.Join(filepath.Dir(receiver.root), "catalog-packages"))
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := cache.LoadContext(t.Context(), digest)
	if err != nil || pkg.Catalog.Models[0].ModelID != "org/root-model" {
		t.Fatalf("projection differs: %+v %v", pkg, err)
	}
}
