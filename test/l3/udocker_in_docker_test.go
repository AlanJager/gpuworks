//go:build docker

package l3

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestUdockerInDockerNativeLifecycle keeps the udocker CLI launcher, still
// selectable with GPUWORKS_UDOCKER_LAUNCHER=udocker, covered.
func TestUdockerInDockerNativeLifecycle(t *testing.T) {
	runUdockerHarness(t, l3RuntimeImage(), "GPUWORKS_UDOCKER_LAUNCHER=udocker")
}

// TestUdockerInDockerRunveilLifecycle runs the same lifecycle with runveil,
// the default launcher, launching PRoot directly.
func TestUdockerInDockerRunveilLifecycle(t *testing.T) {
	output := runUdockerHarness(t, l3RuntimeImage())
	if !strings.Contains(output, "launcher=runveil") {
		t.Fatalf("harness did not run under runveil: %s", output)
	}
}

// TestUdockerInDockerRunveilWithoutPython proves the whole lifecycle -- pull,
// load, create, launch, exec, remove -- needs no Python on the node: the base
// image has none.
func TestUdockerInDockerRunveilWithoutPython(t *testing.T) {
	output := runUdockerHarness(t, l3AlpineImage(t))
	if !strings.Contains(output, "launcher=runveil") {
		t.Fatalf("harness did not run under runveil: %s", output)
	}
}

func runUdockerHarness(t *testing.T, image string, env ...string) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		skipDocker(t, err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		skipDocker(t, err)
	}
	// The udocker CLI launcher needs the full Python image, which includes the
	// curl binary bundled udocker's offline CLI requires.
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if output, pullErr := exec.CommandContext(ctx, "docker", "pull", image).CombinedOutput(); pullErr != nil {
			skipDocker(t, fmt.Errorf("pull %s: %w: %s", image, pullErr, output))
		}
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	harness := filepath.Join(t.TempDir(), "udocker-harness")
	build := exec.Command("go", "build", "-buildvcs=false", "-trimpath", "-o", harness, "./test/l3/cmd/udocker-harness")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+dockerGOARCH(t))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build udocker harness: %v: %s", err, output)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	uid, gid := fmt.Sprint(os.Getuid()), fmt.Sprint(os.Getgid())
	// The registry and workloads live inside this container; no external network is needed.
	args := []string{"run", "--rm", "--network", "none",
		"--cap-add", "SYS_PTRACE", "--security-opt", "seccomp=unconfined",
		"--user", uid + ":" + gid,
		"--volume", harness + ":/usr/local/bin/udocker-harness:ro"}
	for _, entry := range env {
		args = append(args, "--env", entry)
	}
	args = append(args, "--entrypoint", "/usr/local/bin/udocker-harness", image)
	run := exec.CommandContext(ctx, "docker", args...)
	output, err := run.CombinedOutput()
	if err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") != "1" && (strings.Contains(string(output), "Operation not permitted") || strings.Contains(string(output), "ptrace")) {
			t.Skipf("container runtime does not permit proot/ptrace: %v: %s", err, output)
		}
		t.Fatalf("udocker-in-docker lifecycle: %v: %s", err, output)
	}
	if !strings.Contains(string(output), "udocker-harness-load-ok") {
		t.Fatalf("udocker harness skipped the archive load: %s", output)
	}
	if !strings.Contains(string(output), "udocker-harness-ok") {
		t.Fatalf("udocker harness omitted success marker: %s", output)
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "udocker-harness-timing") {
			t.Log(line)
		}
	}
	return string(output)
}

// l3AlpineCandidates are small images without Python, in order of preference.
// Docker Hub is flaky from CI, so a quay.io copy backs it up.
var l3AlpineCandidates = []string{"alpine:3.20", "quay.io/libpod/alpine:latest"}

// l3AlpineImage returns an Alpine image usable by the Docker daemon:
// GPUWORKS_L3_NO_PYTHON_IMAGE if set; else any Alpine tag already present,
// so a runner needs the network at most once; else the first candidate that
// pulls, retrying each a few times because both registries have timed out
// from CI runners.
func l3AlpineImage(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		skipDocker(t, err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		skipDocker(t, err)
	}
	candidates := l3AlpineCandidates
	if env := os.Getenv("GPUWORKS_L3_NO_PYTHON_IMAGE"); env != "" {
		candidates = []string{env}
	} else if local := localAlpineImage(); local != "" {
		return local
	}
	for _, image := range candidates {
		if exec.Command("docker", "image", "inspect", image).Run() == nil {
			return image
		}
	}
	var failures []string
	for attempt := 1; attempt <= 3; attempt++ {
		for _, image := range candidates {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			output, err := exec.CommandContext(ctx, "docker", "pull", image).CombinedOutput()
			cancel()
			if err == nil {
				return image
			}
			failures = append(failures, fmt.Sprintf("attempt %d %s: %v: %s", attempt, image, err, strings.TrimSpace(string(output))))
		}
		time.Sleep(time.Duration(attempt) * 5 * time.Second)
	}
	skipDocker(t, fmt.Errorf("no Alpine image available: %s", strings.Join(failures, "; ")))
	return ""
}

// localAlpineImage returns an Alpine image the daemon already has, of any tag.
func localAlpineImage() string {
	output, err := exec.Command("docker", "images", "--format", "{{.Repository}}:{{.Tag}}",
		"--filter", "reference=alpine", "--filter", "reference=quay.io/libpod/alpine").Output()
	if err != nil {
		return ""
	}
	for _, image := range strings.Fields(string(output)) {
		if !strings.HasSuffix(image, ":<none>") {
			return image
		}
	}
	return ""
}

func l3RuntimeImage() string {
	if image := os.Getenv("GPUWORKS_L3_RUNTIME_BASE_IMAGE"); image != "" {
		return image
	}
	return "python:3.12"
}

func dockerGOARCH(t *testing.T) string {
	t.Helper()
	output, err := exec.Command("docker", "info", "--format", "{{.Architecture}}").Output()
	if err != nil {
		t.Fatalf("inspect docker architecture: %v", err)
	}
	switch strings.TrimSpace(string(output)) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		t.Fatalf("unsupported docker architecture %q", output)
		return ""
	}
}
