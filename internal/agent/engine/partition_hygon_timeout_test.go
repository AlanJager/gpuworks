package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	dockerdrv "gpuworks.ai/gpuworks/internal/agent/runtime/docker"
	"gpuworks.ai/gpuworks/internal/proto"
)

// A late Docker create still owns the native vdev even before Docker inspect
// can see its container. Neither deployment nor probe teardown may release it.
func TestHygonPendingDockerRunRetainsVdevUntilLateCleanup(t *testing.T) {
	e, injector, helper := hygonTestEngine(t)
	driver := dockerdrv.New()
	e.driver = driver
	previous := partitionInjectors[accelerator.BackendHygonLogical]
	partitionInjectors[accelerator.BackendHygonLogical] = injector
	t.Cleanup(func() { partitionInjectors[accelerator.BackendHygonLogical] = previous })
	dir := t.TempDir()
	release, started, removed := filepath.Join(dir, "release"), filepath.Join(dir, "started"), filepath.Join(dir, "removed")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	cli := `#!/bin/sh
set -eu
case "$1" in
run)
 while [ "$1" != --cidfile ]; do shift; done
 cid="$2"
 touch '` + started + `'
 n=0; while [ ! -e '` + release + `' ] && [ "$n" -lt 150 ]; do n=$((n+1)); sleep 0.2; done
 printf late-hygon-id > "$cid"
 echo late-hygon-id;;
rm) printf '%s' "$*" > '` + removed + `';;
inspect) echo 'Error: No such object' >&2; exit 1;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(cli), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	spec := proto.InstanceDesiredState{UUID: "hygon-delayed", GPUIndexes: []int{0}, VRAMLimitBytes: 1536 << 20}
	ws := agentruntime.WorkloadSpec{Name: containerName(spec.UUID), Image: "fixture", Env: map[string]string{}}
	if ok, reason := injector.apply(e, t.Context(), &ws, spec); !ok {
		t.Fatal(reason)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { _, err := driver.Run(ctx, ws); runDone <- err }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Docker CLI never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error=%v", err)
	}
	if err := e.reclaimInstance(t.Context(), spec.UUID); err == nil || !strings.Contains(err.Error(), "pending run") {
		t.Fatalf("premature deployment cleanup: %v", err)
	}
	if err := driver.Remove(t.Context(), ws.Name); err == nil || !strings.Contains(err.Error(), "pending run") {
		t.Fatalf("premature probe cleanup: %v", err)
	}
	if _, err := driver.Run(t.Context(), ws); err == nil || !strings.Contains(err.Error(), "pending run") {
		t.Fatalf("replacement admitted before old creation finished: %v", err)
	}
	if !helper.owned[4] {
		t.Fatal("native vdev released before late Docker create")
	}
	if _, err := os.Stat(filepath.Join(hygonInstanceDir(e, spec.UUID), "vdev4.conf")); err != nil {
		t.Fatalf("snapshot lost before late create: %v", err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		err := e.reclaimInstance(t.Context(), spec.UUID)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("late cleanup never completed: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if helper.owned[4] {
		t.Fatal("native vdev retained after confirmed Docker removal")
	}
	data, err := os.ReadFile(removed)
	if err != nil || string(data) != "rm -f late-hygon-id" {
		t.Fatalf("late container cleanup=%q, %v", data, err)
	}
}
