package nerdctl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadImageFilePinsTheNamespaceAndReportsTheUnpackedRef(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	archive := filepath.Join(dir, "runner.tar")
	if err := os.WriteFile(archive, []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	installFakeNerdctl(t, "printf '%s\\n' \"$*\" >> '"+log+"'\ncase \"$3\" in load) echo 'unpacking gpustack/runner:cuda13.0-vllm0.25.1 (sha256:aabbcc)...'; echo done;; esac")

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
	if strings.TrimSpace(string(args)) != "-n "+Namespace+" load -i "+archive {
		t.Fatalf("nerdctl args = %q, want the gpuworks namespace and the archive path", args)
	}
}

func TestLoadImageFileReportsTheRuntimeErrorText(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "runner.tar")
	if err := os.WriteFile(archive, []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	installFakeNerdctl(t, "case \"$3\" in load) echo 'unexpected EOF' >&2; exit 1;; esac")

	err := New().LoadImageFile(context.Background(), "registry.example/team/runner:v1", archive)
	if err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("LoadImageFile error = %v, want the runtime's own message", err)
	}
}

// The relayed path hands nerdctl a stream; the namespace must still be pinned
// and the unpacked reference still read off stdout.
func TestLoadImageRefReadsAStreamInTheGPUWorksNamespace(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	body := filepath.Join(dir, "stdin")
	installFakeNerdctl(t, "printf '%s\\n' \"$*\" >> '"+log+"'\ncase \"$3\" in load) cat > '"+body+"'; echo 'unpacking gpustack/runner:cuda13.0-vllm0.25.1 (sha256:aabbcc)...'; echo done;; esac")

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
	if strings.TrimSpace(string(args)) != "-n "+Namespace+" load" {
		t.Fatalf("nerdctl args = %q", args)
	}
	streamed, err := os.ReadFile(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(streamed) != "relayed-archive" {
		t.Fatalf("nerdctl read %q from the stream", streamed)
	}
}
