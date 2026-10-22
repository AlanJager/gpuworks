package udocker

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// privateAgentState builds an agent state directory the way a fresh agent
// leaves it: 0700 with a private identity file. t.TempDir has a private
// test-name ancestor that would hide the permission under test, so the fixture
// lives in its own traversable directory.
func privateAgentState(t *testing.T, prefix string) (parent, private string) {
	t.Helper()
	outer, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outer) })
	if err := os.Chmod(outer, 0o755); err != nil {
		t.Fatal(err)
	}
	parent = filepath.Join(outer, "agent-state")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	private = filepath.Join(parent, "identity.json")
	if err := os.WriteFile(private, []byte("private agent identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	return parent, private
}

// The root agent actually executes PRoot as another uid. Permission-bit
// assertions alone cannot prove that the runtime can reach it or that agent
// state stays private. This case needs root and runs explicitly in acceptance.
func TestStateTraversalCrossUIDExecutionAndPrivacy(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("cross-uid execution requires root")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("privacy probe requires python3")
	}
	parent, private := privateAgentState(t, "gpuworks-runtime-parent-")
	const uid, gid = 54321, 54322
	d := New(parent)
	d.runUser, d.runUID, d.runGID = "gpuworks-test-runtime", uid, gid
	if err := d.extractProot(); err != nil {
		t.Fatal(err)
	}
	run := func(program string, args ...string) ([]byte, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, program, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
			Uid: uid, Gid: gid, Groups: []uint32{},
		}}
		return cmd.CombinedOutput()
	}
	if out, err := run(d.prootPath, "--version"); err == nil {
		t.Fatalf("private parent unexpectedly allowed PRoot execution: %s", out)
	}
	if err := d.ensureStateTraversal(); err != nil {
		t.Fatal(err)
	}
	if out, err := run(d.prootPath, "--version"); err != nil || !strings.Contains(string(out), "PRoot") {
		t.Fatalf("runtime cannot execute patched PRoot after provisioning: %s: %v", out, err)
	}
	const probe = `import os, sys
parent = sys.argv[1]
assert os.path.isdir(parent + '/udocker/bin')
for operation in (lambda: os.listdir(parent),
                  lambda: open(parent + '/identity.json', 'rb'),
                  lambda: open(parent + '/runtime-must-not-write', 'wb')):
    try:
        operation()
    except PermissionError:
        continue
    raise AssertionError('runtime gained private state access')
print('runtime traversal and private state protection verified')
`
	if out, err := run(python, "-c", probe, parent); err != nil {
		t.Fatalf("runtime privacy probe failed: %s: %v", out, err)
	}
	assertMode(t, parent, 0o710)
	assertMode(t, private, 0o600)

	// A state directory owned by someone else is reported, never adopted.
	foreign, _ := privateAgentState(t, "gpuworks-foreign-parent-")
	if err := os.Chown(foreign, uid+1, 0); err != nil {
		t.Fatal(err)
	}
	d = New(foreign)
	d.runUser, d.runUID, d.runGID = "gpuworks-test-runtime", uid, gid
	if err := d.ensureStateTraversal(); err == nil || !strings.Contains(err.Error(), "must belong to the agent") {
		t.Fatalf("foreign state directory: err=%v", err)
	}
	assertMode(t, foreign, 0o700)
}

func TestProvisionRuntimeFromPrivateAgentState(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("dedicated runtime provisioning requires root")
	}
	if _, err := user.Lookup("udocker"); err != nil {
		// The test must not create system accounts; acceptance hosts carry one.
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("GPUWORKS_TEST_STRICT=1 requires the udocker account: %v", err)
		}
		t.Skip("acceptance needs an existing udocker account; do not create test accounts")
	}
	t.Setenv(launcherEnv, "runveil")
	parent, private := privateAgentState(t, "gpuworks-provision-parent-")
	runPRoot := func(d *Driver) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := d.ensureProvisioned(ctx); err != nil {
			t.Fatal(err)
		}
		if d.runUser != "udocker" || d.runUID == os.Geteuid() {
			t.Fatal("provisioning did not select a dedicated runtime user")
		}
		out, err := exec.CommandContext(ctx, "runuser", "-u", d.runUser, "--", d.prootPath, "--version").CombinedOutput()
		if err != nil || !strings.Contains(string(out), "PRoot") {
			t.Fatalf("runtime could not execute patched PRoot: %s: %v", out, err)
		}
		assertMode(t, parent, 0o710)
		assertMode(t, private, 0o600)
	}
	// Fresh state, then an agent restart over the prepared state.
	for restart := 0; restart < 2; restart++ {
		runPRoot(New(parent))
	}
	// A state directory reset after provisioning is repaired on the next use
	// by the same, already provisioned driver.
	d := New(parent)
	runPRoot(d)
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	runPRoot(d)
}
