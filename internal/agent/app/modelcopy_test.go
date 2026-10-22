package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

func copyTask(key, source string) proto.ModelFileTask {
	return proto.ModelFileTask{Key: key, Acquisition: proto.ModelAcquireCopy, SourcePath: source,
		Source: "local", SourceID: "org/model", DesiredState: "present"}
}

func TestCopyModelDirResumesAPartialFile(t *testing.T) {
	r := newStorageTestRuntime(t)
	source := filepath.Join(t.TempDir(), "src")
	writeModel(t, source, strings.Repeat("w", 4096))
	target := filepath.Join(t.TempDir(), "dst")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	// Half of the weights were copied before an interruption.
	if err := os.WriteFile(filepath.Join(target, "model.safetensors.part"), []byte(strings.Repeat("w", 2048)), 0o644); err != nil {
		t.Fatal(err)
	}
	r.copyModelDir(context.Background(), copyTask("f1", source), target, target)
	got := r.lastFile(t, "f1")
	if got.State != proto.MFPresent || got.Path != target {
		t.Fatalf("status = %+v", got)
	}
	b, err := os.ReadFile(filepath.Join(target, "model.safetensors"))
	if err != nil || string(b) != strings.Repeat("w", 4096) {
		t.Fatalf("weights = %d bytes err=%v", len(b), err)
	}
	if _, err := os.Stat(filepath.Join(target, "model.safetensors.part")); !os.IsNotExist(err) {
		t.Fatal("partial file left behind")
	}
	if _, ok := validCompletionMarker(target, copyTask("f1", source)); !ok {
		t.Fatal("no valid completion marker after the copy")
	}
}

func TestCopyModelDirFailsOnABadSource(t *testing.T) {
	r := newStorageTestRuntime(t)
	// No layout is required of a model, so a bad source is one that holds
	// nothing or is still being written.
	empty := t.TempDir()
	unfinished := t.TempDir()
	if err := os.WriteFile(filepath.Join(unfinished, "model.gguf.part"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for key, source := range map[string]string{"f2": empty, "f3": "relative/path", "f4": filepath.Join(empty, "absent"), "f6": unfinished} {
		r.copyModelDir(context.Background(), copyTask(key, source), t.TempDir(), t.TempDir())
		if got := r.lastFile(t, key); got.State != proto.MFFailed {
			t.Fatalf("%s: copy from %q = %+v", key, source, got)
		}
	}
}

func TestCopyModelDirStopsSilentlyWhenCanceled(t *testing.T) {
	r := newStorageTestRuntime(t)
	source := filepath.Join(t.TempDir(), "src")
	writeModel(t, source, "w")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.copyModelDir(ctx, copyTask("f5", source), t.TempDir(), t.TempDir())
	for _, st := range r.files {
		if st.Key == "f5" && (st.State == proto.MFFailed || st.State == proto.MFPresent) {
			t.Fatalf("a canceled copy reported %+v", st)
		}
	}
}

func TestInstallStagedModelKeepsAnotherWritersCopy(t *testing.T) {
	task := copyTask("f6", "/unused")
	task.Staging = ".gpuworks/incoming/cp/org/model"
	root := t.TempDir()
	final := filepath.Join(root, "org", "model")
	// Another control plane installed the same model first.
	writeModel(t, final, "w")
	files, err := listCopySource(final)
	if err != nil {
		t.Fatal(err)
	}
	manifest := make([]modelrepo.File, 0, len(files))
	for _, f := range files {
		manifest = append(manifest, modelrepo.File{Path: f.rel, Size: f.size})
	}
	if _, err := writeCompletionMarker(final, task, manifest); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(root, task.Staging)
	writeModel(t, staging, "w")
	if err := installStagedModel(staging, final, task); err != nil {
		t.Fatalf("adopting the same model: %v", err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatal("staging copy kept after adopting the other writer's")
	}
	// A different model at the path is never overwritten.
	other := copyTask("f7", "/unused")
	other.SourceID, other.Staging = "org/other", ".gpuworks/incoming/cp/org/model"
	writeModel(t, staging, "w2")
	if err := installStagedModel(staging, final, other); err == nil || !strings.Contains(err.Error(), "different model") {
		t.Fatalf("different model at the final path = %v", err)
	}
	// An empty final directory is replaced.
	empty := filepath.Join(root, "org", "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installStagedModel(staging, empty, other); err != nil {
		t.Fatalf("install over an empty directory: %v", err)
	}
}

func TestCopyLimiterPaces(t *testing.T) {
	l := newCopyLimiter(64) // 64 KiB/s
	start := time.Now()
	l.wait(context.Background(), 16*1024)
	l.wait(context.Background(), 16*1024)
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
		t.Fatalf("32 KiB at 64 KiB/s took %s", elapsed)
	}
	unlimited := newCopyLimiter(0)
	start = time.Now()
	unlimited.wait(context.Background(), 1<<30)
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("an unlimited copy waited")
	}
}
