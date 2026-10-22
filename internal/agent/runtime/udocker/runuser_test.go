package udocker

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"testing"
	"time"
)

// The lock is what keeps two processes from creating the user at once; each
// flock takes its own open file, so two holders in one process exclude each
// other exactly as two processes do.
func TestRunUserCreationLockIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime-user.lock")
	saved := runUserLockPath
	runUserLockPath = func() string { return path }
	t.Cleanup(func() { runUserLockPath = saved })

	unlock := lockRunUserCreation()
	acquired := make(chan struct{})
	go func() {
		release := lockRunUserCreation()
		close(acquired)
		release()
	}()
	select {
	case <-acquired:
		t.Fatal("a second holder got the lock while the first held it")
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("the lock was not handed over after release")
	}
}

func TestRunUserCreationLockDegradesWithoutALockFile(t *testing.T) {
	saved := runUserLockPath
	runUserLockPath = func() string { return filepath.Join(t.TempDir(), "missing", "dir", "x.lock") }
	t.Cleanup(func() { runUserLockPath = saved })
	// No lock file can be created: provisioning must go on, unserialized.
	lockRunUserCreation()()
}

// An existing account is returned without running useradd, which a non-root
// test process could not run anyway.
func TestEnsureRunUserFindsAnExistingAccount(t *testing.T) {
	saved := runUserLockPath
	runUserLockPath = func() string { return filepath.Join(t.TempDir(), "x.lock") }
	t.Cleanup(func() { runUserLockPath = saved })
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	account, err := ensureRunUser(context.Background(), me.Username)
	if err != nil || account.Uid != me.Uid {
		t.Fatalf("account=%+v err=%v", account, err)
	}
	if os.Geteuid() != 0 {
		if _, err := ensureRunUser(context.Background(), "gpuworks-no-such-user"); err == nil {
			t.Fatal("a missing account that useradd cannot create must be reported")
		}
	}
}

// A workload runs as the runtime user; a login user probing it gets EPERM,
// which must read as alive or `runveil ps` shows running containers as
// exited. pid 1 belongs to root, so a non-root test sees the same EPERM.
func TestProcessAliveCountsOtherUsersProcesses(t *testing.T) {
	if !processAlive(1) {
		t.Fatal("pid 1 reported dead")
	}
	if processAlive(1 << 30) {
		t.Fatal("a pid above pid_max reported alive")
	}
	d := New(t.TempDir())
	root := filepath.Join(d.udockerDir, "containers", "init", "ROOT")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(d.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.stateDir, "init.pid"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	containers, err := d.Containers(context.Background())
	if err != nil || len(containers) != 1 || containers[0].State != ContainerRunning {
		t.Fatalf("containers = %+v err=%v", containers, err)
	}
}
