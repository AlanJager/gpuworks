package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadImageFileHandsDockerThePathAndReportsTheLoadedRef(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	archive := filepath.Join(dir, "runner.tar")
	if err := os.WriteFile(archive, []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	installFakeDocker(t, "printf '%s\\n' \"$*\" >> '"+log+"'\ncase \"$1\" in load) echo 'e2eb06d8af82: Loading layer'; echo 'Loaded image: gpustack/runner:cuda13.0-vllm0.25.1';; esac")

	ref, err := New().LoadImageFileRef(context.Background(), "registry.example/team/runner:v1", archive)
	if err != nil {
		t.Fatalf("LoadImageFileRef: %v", err)
	}
	if ref != "gpustack/runner:cuda13.0-vllm0.25.1" {
		t.Fatalf("loaded ref = %q", ref)
	}
	args, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	// The archive must be read from where it already is: docker is given the
	// path, not a pipe that would mean a second full-size copy.
	if strings.TrimSpace(string(args)) != "load -i "+archive {
		t.Fatalf("docker args = %q", args)
	}
}

func TestLoadImageFileReportsTheRuntimeErrorText(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "runner.tar")
	if err := os.WriteFile(archive, []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	installFakeDocker(t, "case \"$1\" in load) echo 'invalid tar header' >&2; exit 1;; esac")

	err := New().LoadImageFile(context.Background(), "registry.example/team/runner:v1", archive)
	if err == nil || !strings.Contains(err.Error(), "invalid tar header") {
		t.Fatalf("LoadImageFile error = %v, want the runtime's own message", err)
	}
}

func TestLoadImageFileReportsNoRefWhenTheArchiveCarriedNone(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "runner.tar")
	if err := os.WriteFile(archive, []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	installFakeDocker(t, "case \"$1\" in load) echo 'Loaded image ID: sha256:abcdef';; esac")

	ref, err := New().LoadImageFileRef(context.Background(), "registry.example/team/runner:v1", archive)
	if err != nil {
		t.Fatalf("LoadImageFileRef: %v", err)
	}
	if ref != "" {
		t.Fatalf("loaded ref = %q, want none", ref)
	}
}

// A relayed archive is never a file on the node: it arrives as a stream. The
// reference docker names on stdout is the only way that path can learn what
// the archive really carried.
func TestLoadImageRefReadsAStreamAndReportsTheLoadedRef(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	body := filepath.Join(dir, "stdin")
	installFakeDocker(t, "printf '%s\\n' \"$*\" >> '"+log+"'\ncase \"$1\" in load) cat > '"+body+"'; echo 'Loaded image: gpustack/runner:cuda13.0-vllm0.25.1';; esac")

	ref, err := New().LoadImageRef(context.Background(), "registry.example/team/runner:v1", strings.NewReader("relayed-archive"))
	if err != nil {
		t.Fatalf("LoadImageRef: %v", err)
	}
	if ref != "gpustack/runner:cuda13.0-vllm0.25.1" {
		t.Fatalf("loaded ref = %q", ref)
	}
	args, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	// No -i: a stream has no path, and giving docker one would be wrong.
	if strings.TrimSpace(string(args)) != "load" {
		t.Fatalf("docker args = %q", args)
	}
	streamed, err := os.ReadFile(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(streamed) != "relayed-archive" {
		t.Fatalf("docker read %q from the stream", streamed)
	}
}

func TestLoadImageFromAStreamReportsTheRuntimeErrorText(t *testing.T) {
	installFakeDocker(t, "case \"$1\" in load) cat > /dev/null; echo 'invalid tar header' >&2; exit 1;; esac")

	err := New().LoadImage(context.Background(), "registry.example/team/runner:v1", strings.NewReader("junk"))
	if err == nil || !strings.Contains(err.Error(), "invalid tar header") {
		t.Fatalf("LoadImage error = %v, want the runtime's own message", err)
	}
}
