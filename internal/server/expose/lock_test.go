//go:build linux || darwin

package expose

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The CLI and the connector service are not necessarily the same account: the
// CLI often runs as root, the service as the data dir's owner, and both take
// the same locks through the same files. These tests pin what makes that work —
// and what made a real installation report "a connector service is already
// running" for an hour while nothing was running: a lock file the root CLI had
// created 0644 (umask 022), which the service's account could not open.

// TestLockFilesKeepGroupWriteUnderUmask: the mode passed to open is not what
// lands on disk — the creating process's umask decides — so the mode is set
// again after the open, on both locks.
func TestLockFilesKeepGroupWriteUnderUmask(t *testing.T) {
	store := NewStore(t.TempDir())
	prev := syscall.Umask(0o022)
	defer syscall.Umask(prev)

	release, err := store.Lock(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	release()
	release, err = store.SupervisorLock()
	if err != nil {
		t.Fatal(err)
	}
	release()

	for _, name := range []string{".lock", ".supervisor.lock"} {
		info, err := os.Stat(filepath.Join(store.Root(), name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o664 {
			t.Errorf("%s mode = %o, want 664: the group write is what lets the connector service, running as the data dir's owner, open the lock the CLI created", name, got)
		}
	}
}

// TestLockPathRepairsAStrippedMode: a lock that already exists without the
// group write gets it back the next time its owner opens the file.
func TestLockPathRepairsAStrippedMode(t *testing.T) {
	store := NewStore(t.TempDir())
	release, err := store.SupervisorLock()
	if err != nil {
		t.Fatal(err)
	}
	release()
	path := filepath.Join(store.Root(), ".supervisor.lock")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	held, err := store.SupervisorCheck()
	if err != nil || held {
		t.Fatalf("SupervisorCheck = %v, %v, want no holder and no error", held, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o664 {
		t.Fatalf("lock mode = %o, want the stripped group write restored", got)
	}
}

// TestSupervisorCheckTellsHeldFromUnanswerable: a live holder and a lock this
// process cannot examine at all are different answers, and only the first may
// reach the operator as "a connector service is already running".
func TestSupervisorCheckTellsHeldFromUnanswerable(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Root(), ".supervisor.lock")

	release, err := store.SupervisorLock()
	if err != nil {
		t.Fatal(err)
	}
	held, err := store.SupervisorCheck()
	if err != nil || !held {
		t.Fatalf("SupervisorCheck = %v, %v with a holder alive, want held without an error", held, err)
	}
	release()

	// A path that cannot be opened at all, whatever account runs the test:
	// open on a directory reports EISDIR, root or not.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	held, err = store.SupervisorCheck()
	if err == nil || held || errors.Is(err, errLockHeld) {
		t.Fatalf("SupervisorCheck = %v, %v with the lock unopenable, want an error that is not a held lock", held, err)
	}
	if !store.SupervisorHeld() {
		t.Fatal("SupervisorHeld must count an unexaminable lock as held: its callers remove configuration or start a second service on a false")
	}
	err = RunService(t.Context(), store, nil)
	if err == nil {
		t.Fatal("RunService should refuse when the lock cannot be examined")
	}
	if strings.Contains(err.Error(), "already running") {
		t.Fatalf("RunService = %v, must not claim a second service when the lock is unopenable", err)
	}
	if !strings.Contains(err.Error(), "cannot take the hosting lock") {
		t.Fatalf("RunService = %v, want the lock failure named", err)
	}
}
