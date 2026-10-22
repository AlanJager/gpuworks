package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// gpu3 2026-10-20: dockerd took ~90 s to create a container while CI saturated
// the root disk. The launch must say it timed out, and the container the
// daemon finished creating afterwards must not be left behind.
func TestRunTimeoutIsNamedAndTheLateContainerIsRemoved(t *testing.T) {
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	removed := filepath.Join(dir, "removed")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	installFakeDocker(t, `case "$1" in
run) n=0; while [ ! -e '`+release+`' ] && [ "$n" -lt 150 ]; do n=$((n+1)); sleep 0.2; done; echo late-container-id;;
rm) printf '%s\n' "$*" >> '`+removed+`';;
esac`)
	runTimeout, abandonGrace = 100*time.Millisecond, time.Minute
	t.Cleanup(func() { runTimeout, abandonGrace = 0, 0 })

	_, err := New().Run(context.Background(), runtime.WorkloadSpec{Name: "gpuworks-abc", Image: "img"})
	if err == nil || !strings.Contains(err.Error(), "docker run timed out after 100ms") {
		t.Fatalf("err=%v", err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, _ := os.ReadFile(removed)
		if strings.TrimSpace(string(data)) == "rm -f late-container-id" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("late container was not removed; rm calls: %q", data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRemoveTreatsAnAbsentContainerAsRemoved(t *testing.T) {
	installFakeDocker(t, `echo 'Error response from daemon: No such container: gpuworks-gone' >&2; exit 1`)
	if err := New().Remove(context.Background(), "gpuworks-gone"); err != nil {
		t.Fatalf("Remove of an absent container: %v", err)
	}
}

func TestRemoveReportsOtherDaemonErrors(t *testing.T) {
	installFakeDocker(t, `echo 'Error response from daemon: cannot remove container: device or resource busy' >&2; exit 1`)
	if err := New().Remove(context.Background(), "gpuworks-busy"); err == nil {
		t.Fatal("Remove swallowed a real removal failure")
	}
}
