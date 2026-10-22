package modelscan

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateModelDirAcceptsAnyLayout(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"hf":         {"config.json": "{}", "shard/model.safetensors": "weights"},
		"gguf only":  {"IQ2_XS/model-00001-of-00002.gguf": "ab", "IQ2_XS/model-00002-of-00002.gguf": "cd"},
		"mistral":    {"params.json": "{}", "tekken.json": "{}", "consolidated.safetensors": "w"},
		"unknown":    {"engine.plan": "serialized"},
		"weightless": {"config.json": "{}"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			var want int64
			for rel, content := range files {
				write(t, filepath.Join(dir, rel), content)
				want += int64(len(content))
			}
			size, err := ValidateModelDir(dir)
			if err != nil || size != want {
				t.Fatalf("size=%d want %d err=%v", size, want, err)
			}
		})
	}
}

func TestValidateModelDirFollowsRootSymlink(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "model.gguf"), "weights")
	link := filepath.Join(t.TempDir(), "model-link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	size, err := ValidateModelDir(link)
	if err != nil || size <= 0 {
		t.Fatalf("symlinked model dir size=%d err=%v", size, err)
	}
}

// A snapshot directory holds only links into a blob store; the bytes behind
// the links are the model.
func TestValidateModelDirAcceptsLinkedFiles(t *testing.T) {
	blobs := t.TempDir()
	write(t, filepath.Join(blobs, "abc"), "weights")
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(blobs, "abc"), filepath.Join(dir, "model.gguf")); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateModelDir(dir); err != nil {
		t.Fatalf("linked snapshot rejected: %v", err)
	}
}

func TestValidateModelDirRejectsEmpty(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"no files":         {},
		"empty files":      {"model.gguf": ""},
		"only marker":      {CompletionMarker: `{"files":[]}`},
		"only directories": {"sub/.keep": ""},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for rel, content := range files {
				write(t, filepath.Join(dir, rel), content)
			}
			if _, err := ValidateModelDir(dir); !errors.Is(err, errEmptyModelDir) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestValidateModelDirRejectsUnfinishedDownload(t *testing.T) {
	for _, leftover := range []string{"IQ2_XS/model-00002-of-00002.gguf.part", "model.safetensors.part.state"} {
		t.Run(leftover, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "config.json"), "{}")
			write(t, filepath.Join(dir, "IQ2_XS/model-00001-of-00002.gguf"), "ab")
			write(t, filepath.Join(dir, leftover), "partial")
			_, err := ValidateModelDir(dir)
			if err == nil || !strings.Contains(err.Error(), "unfinished download: "+leftover) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestValidateModelDirMissingDir(t *testing.T) {
	if _, err := ValidateModelDir(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error")
	}
}
