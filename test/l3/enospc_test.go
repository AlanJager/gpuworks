//go:build docker

package l3

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDockerDownloadReportsRealENOSPCAndPartialBytes(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		skipDocker(t, err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		skipDocker(t, err)
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	harness := filepath.Join(t.TempDir(), "enospc-harness")
	build := exec.Command("go", "build", "-buildvcs=false", "-trimpath", "-o", harness, "./test/l3/cmd/enospc-harness")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+dockerGOARCH(t))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build ENOSPC harness: %v: %s", err, output)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// The harness serves its download source on container loopback.
	// Only the tmpfs write path is under test; no bridge is needed.
	run := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--tmpfs", "/models:size=1m", "--volume", harness+":/usr/local/bin/enospc-harness:ro", "--entrypoint", "/usr/local/bin/enospc-harness", l3RuntimeImage())
	output, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("real ENOSPC download: %v: %s", err, output)
	}
	if !strings.Contains(string(output), "enospc-harness-ok") {
		t.Fatalf("ENOSPC harness omitted success marker: %s", output)
	}
}
