package udocker

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// playNameProbe makes the name probe fail the way a filesystem does and
// records the names it was asked for.
func playNameProbe(t *testing.T, err error) *[]string {
	t.Helper()
	var patterns []string
	saved := createNameProbe
	createNameProbe = func(dir, pattern string) (*os.File, error) {
		patterns = append(patterns, pattern)
		return nil, &os.PathError{Op: "open", Path: dir + "/" + pattern, Err: err}
	}
	t.Cleanup(func() { createNameProbe = saved })
	return &patterns
}

func TestColonNamesAllowedOnALocalFilesystem(t *testing.T) {
	dir := t.TempDir()
	if err := colonNamesAllowed(dir); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("probe left %v behind", entries)
	}
}

// A probe that cannot run says nothing about file names: it must not stop a
// node that used to work (a full disk at restart, a directory left owned by
// another user) from setting up.
func TestColonNamesAllowedIgnoresFailuresUnrelatedToNames(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EACCES, syscall.ENOSPC, syscall.EROFS} {
		playNameProbe(t, errno)
		if err := colonNamesAllowed(t.TempDir()); err != nil {
			t.Fatalf("%v: err=%v", errno, err)
		}
	}
	playNameProbe(t, syscall.EILSEQ)
	if err := colonNamesAllowed(t.TempDir()); err == nil || !strings.Contains(err.Error(), "does not allow ':'") {
		t.Fatalf("EILSEQ: err=%v", err)
	}
}

// RunPod's /workspace answers a ':' in a file name with EINVAL. The runtime is
// then unusable there, so its setup fails with the reason and what to do, and
// the failure reads as the runtime's own (an image pull stops at it instead of
// blaming every image source in turn).
func TestProvisioningFailsWhereColonsAreRefused(t *testing.T) {
	patterns := playNameProbe(t, syscall.EINVAL)
	t.Setenv(launcherEnv, "")
	d := New(t.TempDir())
	err := d.ensureProvisioned(context.Background())
	for _, want := range []string{d.udockerDir + " does not allow ':' in file names", "dpkg/info", "reinstall the agent with an install directory on a local filesystem"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("provisioning error %v lacks %q", err, want)
		}
	}
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("err=%v does not read as the runtime being unavailable", err)
	}
	if len(*patterns) != 1 || !strings.Contains((*patterns)[0], ":") {
		t.Fatalf("probe patterns = %v, want one name holding ':'", *patterns)
	}
	if state := d.ProvisionState(); state.Provisioned || !strings.Contains(state.LastError, "does not allow ':'") {
		t.Fatalf("provision state = %+v", state)
	}
}
