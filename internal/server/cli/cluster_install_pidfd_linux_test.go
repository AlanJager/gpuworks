package cli

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// requirePidfd skips a test of the installer's owned processes where the
// kernel or the container's seccomp profile has no pidfd_open: the installer
// refuses to manage a process it cannot pin, so there is nothing to observe.
// GPUWORKS_TEST_STRICT=1 turns the missing capability into a failure.
func requirePidfd(t *testing.T) {
	t.Helper()
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err == nil {
		_ = unix.Close(fd)
		return
	}
	if !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EPERM) {
		t.Fatalf("pidfd_open: %v", err)
	}
	if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
		t.Fatalf("pidfd_open is unavailable to this process (%v); GPUWORKS_TEST_STRICT=1 requires it", err)
	}
	t.Skipf("pidfd_open is unavailable to this process (%v)", err)
}
