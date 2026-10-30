package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRunCLI writes a shell script standing in for `docker`/`nerdctl`. Scripts
// that wait on a release file cap their own lifetime (~30 s) so a SIGKILLed
// test binary, which never runs t.Cleanup, does not leave them behind forever.
func fakeRunCLI(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakecli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitForRelease(release string) string {
	return "n=0; while [ ! -e '" + release + "' ] && [ \"$n\" -lt 150 ]; do n=$((n+1)); sleep 0.2; done; "
}

type removeRecorder struct {
	calls chan string
	err   error
}

func newRemoveRecorder(err error) *removeRecorder {
	return &removeRecorder{calls: make(chan string, 4), err: err}
}

func (r *removeRecorder) remove(_ context.Context, target string) error {
	r.calls <- target
	return r.err
}

func (r *removeRecorder) next(t *testing.T) string {
	t.Helper()
	select {
	case target := <-r.calls:
		return target
	case <-time.After(10 * time.Second):
		t.Fatal("abandoned run was never reclaimed")
		return ""
	}
}

func (r *removeRecorder) none(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case target := <-r.calls:
		t.Fatalf("unexpected removal of %q", target)
	case <-time.After(wait):
	}
}

func TestRunDetachedReturnsThePrintedID(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	cli := fakeRunCLI(t, "printf '%s\\n' \"$*\" > '"+argsFile+"'; echo ' abc123 '")
	rec := newRemoveRecorder(nil)
	id, err := RunDetached(context.Background(), DetachedRun{CLI: cli, Args: []string{"run", "-d", "--name", "gpuworks-x", "img"}, Name: "gpuworks-x", Remove: rec.remove})
	if err != nil || id != "abc123" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	args, _ := os.ReadFile(argsFile)
	if strings.TrimSpace(string(args)) != "run -d --name gpuworks-x img" {
		t.Fatalf("argv=%q", args)
	}
	rec.none(t, 100*time.Millisecond)
}

func TestRunDetachedReportsCLIErrorsWithoutReclaiming(t *testing.T) {
	cli := fakeRunCLI(t, "echo 'Error response from daemon: Conflict. The container name is already in use' >&2; exit 125")
	rec := newRemoveRecorder(nil)
	_, err := RunDetached(context.Background(), DetachedRun{CLI: cli, Name: "gpuworks-x", Remove: rec.remove})
	if err == nil || !strings.Contains(err.Error(), "exit status 125") || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("err=%v", err)
	}
	// A run that returned is the caller's to clean up (it inspects by name);
	// reclaiming here would race a start the caller retries under that name.
	rec.none(t, 100*time.Millisecond)
}

func TestRunDetachedRejectsAnEmptyID(t *testing.T) {
	cli := fakeRunCLI(t, "exit 0")
	if _, err := RunDetached(context.Background(), DetachedRun{CLI: cli, Name: "gpuworks-x"}); err == nil || !strings.Contains(err.Error(), "empty container id") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunDetachedReportsAMissingCLI(t *testing.T) {
	_, err := RunDetached(context.Background(), DetachedRun{CLI: filepath.Join(t.TempDir(), "absent"), Name: "gpuworks-x"})
	if err == nil || !strings.Contains(err.Error(), "run:") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunDetachedTimeoutNamesTheTimeoutAndRemovesTheLateContainerByID(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	cli := fakeRunCLI(t, waitForRelease(release)+"echo late-id")
	rec := newRemoveRecorder(nil)
	started := time.Now()
	_, err := RunDetached(context.Background(), DetachedRun{CLI: cli, Name: "gpuworks-x", Remove: rec.remove, Timeout: 200 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "run timed out after 200ms") {
		t.Fatalf("err=%v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("timeout returned after %s", elapsed)
	}
	// Nothing is removed while the daemon may still be creating it.
	rec.none(t, 300*time.Millisecond)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := rec.next(t); got != "late-id" {
		t.Fatalf("removed %q, want the id the late run printed", got)
	}
}

func TestRunDetachedTimeoutRemovesByNameWhenTheLateRunFails(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	// docker run -d that created the container but failed to start it prints
	// no id, yet leaves the container behind under its name.
	cli := fakeRunCLI(t, waitForRelease(release)+"echo 'failed to start' >&2; exit 125")
	rec := newRemoveRecorder(nil)
	if _, err := RunDetached(context.Background(), DetachedRun{CLI: cli, Name: "gpuworks-x", Remove: rec.remove, Timeout: 100 * time.Millisecond}); err == nil {
		t.Fatal("expected a timeout")
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := rec.next(t); got != "gpuworks-x" {
		t.Fatalf("removed %q, want the container name", got)
	}
}

func TestRunDetachedKillsARunThatNeverReturnsAndRemovesByName(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	marker := filepath.Join(t.TempDir(), "finished")
	cli := fakeRunCLI(t, waitForRelease(release)+"touch '"+marker+"'; echo never-printed")
	rec := newRemoveRecorder(nil)
	if _, err := RunDetached(context.Background(), DetachedRun{CLI: cli, Name: "gpuworks-x", Remove: rec.remove, Timeout: 100 * time.Millisecond, AbandonGrace: 300 * time.Millisecond}); err == nil {
		t.Fatal("expected a timeout")
	}
	if got := rec.next(t); got != "gpuworks-x" {
		t.Fatalf("removed %q, want the container name", got)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the CLI ran to completion instead of being killed after the grace period")
	}
}

func TestRunDetachedCancellationReclaimsTheContainer(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	cli := fakeRunCLI(t, waitForRelease(release)+"echo cancelled-id")
	rec := newRemoveRecorder(errors.New("daemon unreachable"))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err := RunDetached(ctx, DetachedRun{CLI: cli, Name: "gpuworks-x", Remove: rec.remove, Timeout: time.Minute})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A failing removal is only logged; the attempt itself must still happen.
	if got := rec.next(t); got != "cancelled-id" {
		t.Fatalf("removed %q", got)
	}
}

// A timeout can hide a name collision until the CLI returns. A recorded-ID
// runtime must not reclaim the pre-existing container under that name.
func TestRunDetachedTimedOutNameCollisionDoesNotRemoveExistingContainer(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	cli := fakeRunCLI(t, waitForRelease(release)+"echo 'container name already in use' >&2; exit 125")
	rec := newRemoveRecorder(nil)
	consumed := make(chan struct{})
	_, err := RunDetached(context.Background(), DetachedRun{
		CLI: cli, Name: "existing-container", Remove: rec.remove, Timeout: 50 * time.Millisecond,
		CreatedID: func() (string, error) { close(consumed); return "", nil },
	})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err=%v", err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-consumed:
	case <-time.After(10 * time.Second):
		t.Fatal("creation record was never consumed")
	}
	rec.none(t, 100*time.Millisecond)
}

func TestRunDetachedTimeoutPreservesObservedDriverErrorWithoutGuessingCause(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	detail := "nvidia-container-cli: driver rpc error: timed out"
	cli := fakeRunCLI(t, "echo '"+detail+"' >&2\n"+waitForRelease(release)+"echo late-id")
	rec := newRemoveRecorder(nil)
	_, err := RunDetached(context.Background(), DetachedRun{CLI: cli, Name: "gpuworks-x", Remove: rec.remove, Timeout: 200 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), detail) || strings.Contains(err.Error(), "daemon slow or overloaded") {
		t.Fatalf("timeout must retain observed failure without guessing its cause: %v", err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := rec.next(t); got != "late-id" {
		t.Fatalf("removed %q, want late-id", got)
	}
}
