//go:build docker

package l3

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDockerEnvironmentAvailable(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("docker is required in strict mode: %v", err)
		}
		t.Skipf("docker unavailable: %v", err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("docker daemon unavailable in strict mode: %v", err)
		}
		t.Skipf("docker daemon unavailable: %v", err)
	}
}

func TestFakeEngineImageSmoke(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		skipDocker(t, err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		skipDocker(t, fmt.Errorf("docker daemon unavailable: %w", err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	if err := ensureFakeImage(t, root); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	// The budget is on what the image costs to ship, so measure the saved
	// archive. `docker image inspect .Size` is not comparable across daemons:
	// the containerd image store adds the compressed layer blobs to the unpacked
	// size, which reports the same image about 50% larger than the classic store.
	save := exec.CommandContext(ctx, "docker", "save", "gpuworks/fake-engine:test")
	archive, err := save.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := save.Start(); err != nil {
		t.Fatalf("docker save fake-engine: %v", err)
	}
	size, copyErr := io.Copy(io.Discard, archive)
	if err := save.Wait(); err != nil || copyErr != nil {
		t.Fatalf("docker save fake-engine: wait=%v copy=%v", err, copyErr)
	}
	if size == 0 || size >= 10<<20 {
		t.Fatalf("fake-engine saved image size=%d bytes, want >0 and <10 MiB", size)
	}
	modelDir := t.TempDir()
	if err := os.WriteFile(modelDir+"/config.json", []byte(`{"model_type":"qwen3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	container := fmt.Sprintf("gpuworks-l3-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", container).Run() })
	// This smoke case exercises HTTP from the host. Host networking avoids a
	// dependency on the daemon's default bridge while keeping those assertions.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
	_ = listener.Close()
	run := exec.CommandContext(ctx, "docker", "run", "-d", "--network", "host", "--name", container, "-v", modelDir+":/model:ro", "gpuworks/fake-engine:test", "--model", "/model", "--port", port)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("start fake engine: %v: %s", err, out)
	}
	if out, err := exec.CommandContext(ctx, "docker", "inspect", container, "--format", "{{.State.Running}}").Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Fatalf("fake engine container not running: %v %s", err, out)
	}
	base := "http://127.0.0.1:" + port
	get := func(path string) (int, []byte) {
		t.Helper()
		resp, reqErr := http.Get(base + path)
		if reqErr != nil {
			t.Fatalf("GET %s: %v", path, reqErr)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		return resp.StatusCode, body
	}
	// `docker run -d` returns before the engine listens; until then the
	// host port refuses the connection. Wait for it, then assert through get as usual.
	for ctx.Err() == nil {
		if resp, probeErr := http.Get(base + "/health"); probeErr == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if status, _ := get("/health"); status != http.StatusOK {
		t.Fatalf("engine health status=%d", status)
	}
	if status, body := get("/v1/models"); status != http.StatusOK || !strings.Contains(string(body), "/model") {
		t.Fatalf("engine models status=%d body=%s", status, body)
	}
	chat, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"/model","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	chatBody, _ := io.ReadAll(chat.Body)
	chat.Body.Close()
	if chat.StatusCode != http.StatusOK || !strings.Contains(string(chatBody), "choices") {
		t.Fatalf("chat status=%d body=%s", chat.StatusCode, chatBody)
	}
	if status, body := get("/is_sleeping"); status != http.StatusOK || !strings.Contains(string(body), "false") {
		t.Fatalf("initial sleep status=%d body=%s", status, body)
	}
	if status, _ := get("/sleep"); status != http.StatusOK {
		t.Fatalf("sleep status=%d", status)
	}
	if status, body := get("/is_sleeping"); status != http.StatusOK || !strings.Contains(string(body), "true") {
		t.Fatalf("sleeping status=%d body=%s", status, body)
	}
	if status, _ := get("/wake_up"); status != http.StatusOK {
		t.Fatalf("wake status=%d", status)
	}
	argvPath := filepath.Join(t.TempDir(), "argv.json")
	if out, err := exec.CommandContext(ctx, "docker", "cp", container+":/tmp/fake-engine-argv.json", argvPath).CombinedOutput(); err != nil {
		t.Fatalf("copy recorded argv: %v %s", err, out)
	}
	argv, err := os.ReadFile(argvPath)
	if err != nil || !strings.Contains(string(argv), "--model") || !strings.Contains(string(argv), "/model") {
		t.Fatalf("recorded argv=%s err=%v", argv, err)
	}
}

func TestFakeEngineFailureInjectionExitsDeterministically(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		skipDocker(t, err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		skipDocker(t, err)
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	if err := ensureFakeImage(t, root); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	modelDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"), []byte(`{"model_type":"qwen3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		env  string
		code string
	}{
		{name: "crash", env: "FAKE_ENGINE_CRASH_ON_START=1", code: "42"},
		{name: "oom", env: "FAKE_ENGINE_OOM_EXIT=1", code: "137"},
	} {
		container := "gpuworks-l3-" + tc.name + "-" + fmt.Sprint(time.Now().UnixNano())
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", container).Run() })
		run := exec.Command("docker", "run", "-d", "--network", "none", "--name", container, "-v", modelDir+":/model:ro", "-e", tc.env, "gpuworks/fake-engine:test", "--model", "/model", "--port", "8000")
		if out, err := run.CombinedOutput(); err != nil {
			t.Fatalf("start %s: %v %s", tc.name, err, out)
		}
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Second)
		wait, err := exec.CommandContext(waitCtx, "docker", "wait", container).Output()
		waitCancel()
		if err != nil || strings.TrimSpace(string(wait)) != tc.code {
			t.Fatalf("%s exit=%q err=%v", tc.name, wait, err)
		}
		if tc.name == "oom" {
			logs, logErr := exec.Command("docker", "logs", container).CombinedOutput()
			if logErr != nil || !strings.Contains(string(logs), "torch.cuda.OutOfMemoryError") {
				t.Fatalf("oom logs=%q err=%v", logs, logErr)
			}
		}
		_ = exec.Command("docker", "rm", container).Run()
	}
}

// TestRegistryFakeEngineDigestPull is opt-in because registry credentials and
// network access are intentionally not part of the default test pyramid. The
// current public acceptance source is Quay; an environment override remains
// useful for testing another registry without changing the default gate.
func TestRegistryFakeEngineDigestPull(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_NET") != "1" {
		t.Skip("set GPUWORKS_TEST_NET=1 to exercise the optional registry path")
	}
	ref := strings.TrimSpace(os.Getenv("GPUWORKS_FAKE_ENGINE_REF"))
	if ref == "" {
		ref = "quay.io/zstackai/fake-engine@sha256:d74179cbfc883530e90dc63c1c3242146f3ed2b319fa6145e97e9891ecaaa2f6"
	}
	if !strings.Contains(ref, "@sha256:") {
		t.Fatalf("GPUWORKS_FAKE_ENGINE_REF must be immutable (@sha256:...), got %q", ref)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := exec.LookPath("docker"); err != nil {
		skipDocker(t, err)
	}
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		skipDocker(t, err)
	}
	pull := exec.CommandContext(ctx, "docker", "pull", ref)
	// Credential helpers can outlive the Docker CLI and retain its output
	// pipes. Bound pipe draining as well as the CLI process itself.
	pull.WaitDelay = 5 * time.Second
	if out, err := pull.CombinedOutput(); err != nil {
		t.Fatalf("pull %s: %v (context: %v): %s", ref, err, ctx.Err(), out)
	}
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", ref, "--format", "{{index .RepoDigests 0}}").Output()
	if err != nil || !strings.Contains(string(out), "@sha256:") {
		t.Fatalf("pulled image has no immutable digest: %q err=%v", out, err)
	}
}
