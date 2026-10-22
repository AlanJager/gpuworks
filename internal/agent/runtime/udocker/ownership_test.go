package udocker

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// refuseOwnership plays a filesystem that answers chown the way RunPod's NFS
// volume does, and records what was asked of it.
func refuseOwnership(t *testing.T) *[]string {
	t.Helper()
	var asked []string
	saved := lchown
	lchown = func(name string, uid, gid int) error {
		asked = append(asked, name)
		return &os.PathError{Op: "lchown", Path: name, Err: syscall.EPERM}
	}
	t.Cleanup(func() { lchown = saved })
	return &asked
}

func TestOwnershipTransferableReportsARefusingFilesystem(t *testing.T) {
	dir := t.TempDir()
	asked := refuseOwnership(t)
	refused, err := ownershipTransferable(dir, 4242, 4242)
	if err != nil || refused == nil || !errors.Is(refused, syscall.EPERM) || !strings.Contains(refused.Error(), dir+" refuses to change file ownership") {
		t.Fatalf("refused=%v err=%v", refused, err)
	}
	if len(*asked) != 1 || filepath.Dir((*asked)[0]) != dir {
		t.Fatalf("probe asked %v, want one file under %s", *asked, dir)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("probe file left behind: %v", entries)
	}
}

func TestOwnershipTransferableAcceptsOwnIdentity(t *testing.T) {
	dir := t.TempDir()
	if refused, err := ownershipTransferable(dir, os.Geteuid(), os.Getegid()); refused != nil || err != nil {
		t.Fatalf("refused=%v err=%v", refused, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("probe file left behind: %v", entries)
	}
}

// A probe that could not run (here: a full disk) is not a refusal.
func TestOwnershipProbeFailureIsNotARefusal(t *testing.T) {
	failProbe(t, syscall.ENOSPC)
	refused, err := ownershipTransferable(t.TempDir(), 4242, 4242)
	if refused != nil || !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("refused=%v err=%v", refused, err)
	}
}

func failProbe(t *testing.T, errno syscall.Errno) {
	t.Helper()
	saved := createProbe
	createProbe = func(dir, pattern string) (*os.File, error) {
		return nil, &os.PathError{Op: "open", Path: filepath.Join(dir, pattern), Err: errno}
	}
	t.Cleanup(func() { createProbe = saved })
}

// actAsRoot makes the run-user decision see a root agent whose dedicated
// account is the test's own user, so it runs without root or new accounts.
func actAsRoot(t *testing.T) {
	t.Helper()
	me, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	savedEUID, savedLookup := geteuid, lookupRunUser
	geteuid = func() int { return 0 }
	lookupRunUser = func(context.Context, string) (*user.User, error) { return me, nil }
	t.Cleanup(func() { geteuid, lookupRunUser = savedEUID, savedLookup })
}

func TestRootAgentRunsWorkloadsAsTheDedicatedUser(t *testing.T) {
	actAsRoot(t)
	d := New(t.TempDir())
	d.runveil = true
	for _, dir := range []string{d.udockerDir, d.stateDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.decideRunUser(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.runUser != dedicatedRunUser || d.runUID != os.Getuid() || d.track.fallback != "" {
		t.Fatalf("run identity = %q uid %d fallback %q", d.runUser, d.runUID, d.track.fallback)
	}
}

// The field case: the runtime directories refuse ownership changes, so the
// workloads run as the agent and the reason is recorded for the doctor.
func TestRootAgentFallsBackWhenOwnershipIsRefused(t *testing.T) {
	actAsRoot(t)
	refuseOwnership(t)
	d := New(t.TempDir())
	d.runveil = true
	for _, dir := range []string{d.udockerDir, d.stateDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.decideRunUser(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.runUser != "" || d.runUID != 0 || !strings.Contains(d.track.fallback, "refuses to change file ownership") {
		t.Fatalf("run identity = %q uid %d fallback %q", d.runUser, d.runUID, d.track.fallback)
	}
}

// A probe that cannot run fails the setup instead of moving the workloads to
// root: a full disk at restart must not change whom they run as.
func TestRootAgentKeepsItsIdentityWhenTheOwnershipProbeCannotRun(t *testing.T) {
	actAsRoot(t)
	failProbe(t, syscall.ENOSPC)
	d := New(t.TempDir())
	d.runveil = true
	err := d.decideRunUser(context.Background())
	if !errors.Is(err, syscall.ENOSPC) || d.track.fallback != "" || d.runUser != dedicatedRunUser {
		t.Fatalf("err=%v run user %q fallback %q", err, d.runUser, d.track.fallback)
	}
}

// An account that cannot be looked up or created (no useradd in the image)
// is a reason to fall back, as before.
func TestRootAgentFallsBackWithoutTheDedicatedAccount(t *testing.T) {
	actAsRoot(t)
	lookupRunUser = func(context.Context, string) (*user.User, error) {
		return nil, user.UnknownUserError(dedicatedRunUser)
	}
	d := New(t.TempDir())
	d.runveil = true
	if err := d.decideRunUser(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.runUser != "" || !strings.Contains(d.track.fallback, "unknown user") {
		t.Fatalf("run user %q fallback %q", d.runUser, d.track.fallback)
	}
}

// A root agent whose state lives on a filesystem that refuses chown runs its
// workloads as itself instead of failing every launch with "prepare udocker
// ownership".
func TestProvisionFallsBackToAgentUserWhenOwnershipIsRefused(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("dedicated runtime provisioning requires root")
	}
	if _, err := user.Lookup("udocker"); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("GPUWORKS_TEST_STRICT=1 requires the udocker account: %v", err)
		}
		t.Skip("acceptance needs an existing udocker account; do not create test accounts")
	}
	t.Setenv(launcherEnv, "runveil")
	refuseOwnership(t)
	d := New(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := d.ensureProvisioned(ctx); err != nil {
		t.Fatalf("provisioning failed instead of falling back: %v", err)
	}
	if d.runUser != "" || d.runUID != 0 || d.runGID != 0 {
		t.Fatalf("run identity = %q %d:%d, want the agent's own", d.runUser, d.runUID, d.runGID)
	}
	owner, err := d.MountOwner()
	if err != nil || owner.UID != 0 || owner.Username != "" {
		t.Fatalf("mount owner = %+v err=%v", owner, err)
	}
}
