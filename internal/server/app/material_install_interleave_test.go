//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
)

// observedMaterialContext cancels when a real staged filesystem state appears.
// The caller still runs the production copy/validation/publish path; no link or
// package-store operation is replaced by a test stub.
type observedMaterialContext struct {
	context.Context
	cancel   context.CancelFunc
	ready    func() bool
	trigger  int
	observed int
}

func (c *observedMaterialContext) Err() error {
	if err := c.Context.Err(); err != nil {
		return err
	}
	if c.ready() {
		c.observed++
		if c.observed == c.trigger {
			c.cancel()
		}
	}
	return c.Context.Err()
}

func TestReceivedMaterialCancellationAfterCopyBeforeLinkCanRetry(t *testing.T) {
	dataDir := t.TempDir()
	replicaDir := filepath.Join(dataDir, "material-replicas")
	if err := os.Mkdir(replicaDir, 0700); err != nil {
		t.Fatal(err)
	}
	receiver, err := newMaterialReceiver(replicaDir, 2)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("verified bytes waiting at the consumer publication boundary")
	digest := materialTestDigest(body)
	if _, err := receiver.receive(t.Context(), digest, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	stageDir := filepath.Join(dataDir, materialStageDirectory)
	if err := os.Mkdir(stageDir, 0700); err != nil {
		t.Fatal(err)
	}
	source, err := os.OpenRoot(replicaDir)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := os.OpenRoot(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &observedMaterialContext{Context: base, cancel: cancel, trigger: 2, ready: func() bool {
		entries, err := os.ReadDir(stageDir)
		if err != nil {
			return false
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), ".material-") {
				continue
			}
			info, err := entry.Info()
			if err == nil && info.Mode().IsRegular() && info.Size() == int64(len(body)) {
				return true
			}
		}
		return false
	}}
	name := "model-logos/interleave.png"
	if err := copyReceivedMaterial(ctx, source, target, name, digest, int64(len(body)), true); !errors.Is(err, context.Canceled) || ctx.observed < 2 {
		t.Fatalf("completed stage was not canceled before publication: observations=%d err=%v", ctx.observed, err)
	}
	if _, err := os.Lstat(filepath.Join(dataDir, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled stage published a consumer file: %v", err)
	}
	if entries, err := os.ReadDir(stageDir); err != nil || len(entries) != 0 {
		t.Fatalf("canceled copy left a temporary stage: %v %v", entries, err)
	}
	if err := copyReceivedMaterial(t.Context(), source, target, name, digest, int64(len(body)), true); err != nil {
		t.Fatalf("retry could not publish verified bytes: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dataDir, name)); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("retry published different consumer bytes: %q %v", got, err)
	}
}

func TestReceivedMaterialCancellationAfterLinkFinishesStageCleanup(t *testing.T) {
	dataDir := t.TempDir()
	replicaDir := filepath.Join(dataDir, "material-replicas")
	if err := os.Mkdir(replicaDir, 0700); err != nil {
		t.Fatal(err)
	}
	receiver, err := newMaterialReceiver(replicaDir, 2)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("consumer bytes linked just before cancellation")
	digest := materialTestDigest(body)
	if _, err := receiver.receive(t.Context(), digest, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	stageDir := filepath.Join(dataDir, materialStageDirectory)
	if err := os.Mkdir(stageDir, 0700); err != nil {
		t.Fatal(err)
	}
	source, err := os.OpenRoot(replicaDir)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := os.OpenRoot(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	name := "model-logos/linked.png"
	path := filepath.Join(dataDir, name)
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &observedMaterialContext{Context: base, cancel: cancel, trigger: 1, ready: func() bool {
		_, err := os.Lstat(path)
		return err == nil
	}}
	if err := copyReceivedMaterial(ctx, source, target, name, digest, int64(len(body)), true); !errors.Is(err, context.Canceled) || ctx.observed != 1 {
		t.Fatalf("linked consumer file falsely acknowledged cancellation: observations=%d err=%v", ctx.observed, err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("linked file was not retained for retry: %q %v", got, err)
	}
	if stages, err := os.ReadDir(stageDir); err != nil || len(stages) != 0 {
		t.Fatalf("published file left its staging name: %v %v", stages, err)
	}
	if err := copyReceivedMaterial(t.Context(), source, target, name, digest, int64(len(body)), true); err != nil {
		t.Fatalf("linked file was not accepted on retry: %v", err)
	}
}

func TestReceivedPackageCancellationInsidePutLeavesNoFalseInstallation(t *testing.T) {
	dataDir := t.TempDir()
	replicaDir := filepath.Join(dataDir, "material-replicas")
	if err := os.Mkdir(replicaDir, 0700); err != nil {
		t.Fatal(err)
	}
	receiver, err := newMaterialReceiver(replicaDir, 2)
	if err != nil {
		t.Fatal(err)
	}
	model := inferencecatalog.InferenceModel{ModelID: "org/model", DisplayTitle: "Model", Specs: []inferencecatalog.InferenceSpec{{Name: "default", Engine: "vllm", Image: "vllm/vllm-openai:latest", Weights: inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: "org/model"}}}, GPUs: inferencecatalog.GPUSelector{Vendor: "nvidia", CC: ">=8.0"}}}}
	modelYAML, err := inferencecatalog.MarshalModelYAML(model)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"manifest.yaml":                 []byte("schema_version: 1\npublished_at: 2026-09-21T00:00:00Z\nmodels: 1\nspecs: 1\n"),
		"models/org/model/model.yaml":   modelYAML,
		"models/org/model/README.en.md": []byte("original documentation"),
	}
	fixture := fsFixture(files)
	cache, err := packages.New(filepath.Join(t.TempDir(), "catalog-packages"))
	if err != nil {
		t.Fatal(err)
	}
	packageDigest, _, err := cache.Put(fixture)
	if err != nil {
		t.Fatal(err)
	}
	manifest := materialManifest{SourceMemberID: 1}
	for name, body := range files {
		digest := materialTestDigest(body)
		if _, err := receiver.receive(t.Context(), digest, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		manifest.Files = append(manifest.Files, materialManifestFile{Class: MaterialCatalogPackage, Path: "catalog-packages/" + packageDigest + "/" + name, SHA256: digest, Bytes: int64(len(body))})
	}
	packageRoot := filepath.Join(dataDir, "catalog-packages")
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &observedMaterialContext{Context: base, cancel: cancel, trigger: 1, ready: func() bool {
		stages, err := os.ReadDir(packageRoot)
		if err != nil {
			return false
		}
		for _, stage := range stages {
			if !stage.IsDir() || !strings.HasPrefix(stage.Name(), ".package-") {
				continue
			}
			complete := true
			for name, body := range files {
				info, err := os.Stat(filepath.Join(packageRoot, stage.Name(), filepath.FromSlash(name)))
				if err != nil || info.Size() != int64(len(body)) {
					complete = false
					break
				}
			}
			if complete {
				return true
			}
		}
		return false
	}}
	if err := materializeReceivedManifest(ctx, receiver, manifest); !errors.Is(err, context.Canceled) || ctx.observed == 0 {
		t.Fatalf("package Put did not stop after complete staging: observations=%d err=%v", ctx.observed, err)
	}
	if _, err := os.Lstat(filepath.Join(packageRoot, packageDigest)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled package was reported installed: %v", err)
	}
	if stages, err := os.ReadDir(packageRoot); err != nil || len(stages) != 0 {
		t.Fatalf("canceled package retained temporary or published directories: %v %v", stages, err)
	}
	if err := materializeReceivedManifest(t.Context(), receiver, manifest); err != nil {
		t.Fatalf("package installation could not retry: %v", err)
	}
	if err := checkInstalledMaterialManifest(t.Context(), receiver, manifest); err != nil {
		t.Fatalf("retried package lacks consumer proof: %v", err)
	}
}

func fsFixture(files map[string][]byte) fs.FS {
	fixture := make(fstest.MapFS, len(files))
	for name, body := range files {
		fixture[name] = &fstest.MapFile{Data: body}
	}
	return fixture
}
