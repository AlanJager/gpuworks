package udocker

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// stateUnderPrivateParent builds the field layout: an agent state directory
// below a 0700 operator directory (a private home), under a traversable root.
// The returned driver's state directory is base; blocker is the 0700 one.
func stateUnderPrivateParent(t *testing.T) (d *Driver, blocker string) {
	t.Helper()
	// t.TempDir has a private test-name ancestor of its own, which would hide
	// the one under test, so the fixture lives in its own traversable root.
	outer, err := os.MkdirTemp("/tmp", "gpuworks-stateaccess-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outer) })
	if err := os.Chmod(outer, 0o755); err != nil {
		t.Fatal(err)
	}
	blocker = filepath.Join(outer, "home")
	base := filepath.Join(blocker, "gpuworks")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocker, 0o700); err != nil {
		t.Fatal(err)
	}
	return New(base), blocker
}

// runtimeAccount is a dedicated account that is not the test's own user, so
// the test needs no root: the 0700 directories belong to the test user and the
// runtime account falls in the "other" class.
func runtimeAccount(t *testing.T) *user.User {
	t.Helper()
	return &user.User{Uid: strconv.Itoa(os.Geteuid() + 1000), Gid: strconv.Itoa(os.Getegid() + 1000), Username: dedicatedRunUser}
}

func actAsRootWithAccount(t *testing.T, account *user.User, lookupErr error) {
	t.Helper()
	savedEUID, savedLookup := geteuid, lookupExistingUser
	geteuid = func() int { return 0 }
	lookupExistingUser = func(string) (*user.User, error) {
		if lookupErr != nil {
			return nil, lookupErr
		}
		return account, nil
	}
	t.Cleanup(func() { geteuid, lookupExistingUser = savedEUID, savedLookup })
}

// The field failure: a root agent whose state directory is under a private
// home. Before the first provisioning (or after a failed one) the doctor has
// no run identity yet and judges the dedicated account.
func TestStateAccessProblemNamesThePrivateAncestorBeforeProvisioning(t *testing.T) {
	d, blocker := stateUnderPrivateParent(t)
	account := runtimeAccount(t)
	actAsRootWithAccount(t, account, nil)

	stateDir, blocked := d.StateAccessProblem()
	if blocked == nil {
		t.Fatal("a 0700 ancestor was not reported")
	}
	if stateDir != filepath.Dir(d.udockerDir) || blocked.Dir != blocker || blocked.Mode != 0o700 {
		t.Fatalf("state dir %q, blocked %+v", stateDir, blocked)
	}
	if blocked.Owner.Username != dedicatedRunUser || strconv.Itoa(blocked.Owner.UID) != account.Uid || strconv.Itoa(blocked.Owner.GID) != account.Gid {
		t.Fatalf("judged identity = %+v", blocked.Owner)
	}
	if got := blocked.FixCommand(); got != "chmod o+x "+blocker {
		t.Fatalf("fix = %q", got)
	}
}

// The account does not exist yet: the install creates it with no ownership and
// no group of the directory, so only the "other" bits can let it through.
func TestStateAccessProblemBeforeTheAccountExistsJudgesOthers(t *testing.T) {
	d, blocker := stateUnderPrivateParent(t)
	actAsRootWithAccount(t, nil, user.UnknownUserError(dedicatedRunUser))

	_, blocked := d.StateAccessProblem()
	if blocked == nil || blocked.Dir != blocker || blocked.Owner.UID != -1 || blocked.Owner.Username != dedicatedRunUser {
		t.Fatalf("blocked = %+v", blocked)
	}
	if err := os.Chmod(blocker, 0o701); err != nil {
		t.Fatal(err)
	}
	if _, blocked := d.StateAccessProblem(); blocked != nil {
		t.Fatalf("a directory open to others still reported: %+v", blocked)
	}
}

// Opening the directory, by group for the runtime's primary group or by
// others, clears the finding.
func TestStateAccessProblemClearsOnceTheAncestorIsOpened(t *testing.T) {
	d, blocker := stateUnderPrivateParent(t)
	actAsRootWithAccount(t, runtimeAccount(t), nil)
	if _, blocked := d.StateAccessProblem(); blocked == nil {
		t.Fatal("not reported while closed")
	}
	if err := os.Chmod(blocker, 0o711); err != nil {
		t.Fatal(err)
	}
	if _, blocked := d.StateAccessProblem(); blocked != nil {
		t.Fatalf("reported after chmod o+x: %+v", blocked)
	}
}

// After provisioning the verdict is about the identity the workloads really
// run as: a fallback to the agent's own user needs no grant.
func TestStateAccessProblemFollowsTheProvisionedIdentity(t *testing.T) {
	d, blocker := stateUnderPrivateParent(t)
	account := runtimeAccount(t)
	uid, _ := strconv.Atoi(account.Uid)
	gid, _ := strconv.Atoi(account.Gid)
	// A lookup that would mislead proves the provisioned identity wins.
	actAsRootWithAccount(t, nil, errors.New("must not be consulted"))

	d.track.succeeded(dedicatedRunUser, uid, gid)
	if _, blocked := d.StateAccessProblem(); blocked == nil || blocked.Dir != blocker || blocked.Owner.UID != uid {
		t.Fatalf("provisioned dedicated user: %+v", blocked)
	}

	d.track.succeeded("", os.Geteuid(), os.Getegid())
	if stateDir, blocked := d.StateAccessProblem(); blocked != nil || stateDir != "" {
		t.Fatalf("workloads run as the agent, yet %q reported %+v", stateDir, blocked)
	}
}

func TestStateAccessProblemIsNothingForANonRootAgent(t *testing.T) {
	d, _ := stateUnderPrivateParent(t)
	savedEUID := geteuid
	geteuid = func() int { return 1000 }
	t.Cleanup(func() { geteuid = savedEUID })
	if _, blocked := d.StateAccessProblem(); blocked != nil {
		t.Fatalf("a non-root agent runs workloads as itself: %+v", blocked)
	}
}

// A missing path above the state directory is not "not traversable": the
// verdict stays the traversal one, and an unreadable chain is not reported as
// a blocked directory.
func TestStateAccessProblemIgnoresAChainItCannotStat(t *testing.T) {
	actAsRootWithAccount(t, runtimeAccount(t), nil)
	d := New("/nonexistent-gpuworks-root/a/b")
	if _, blocked := d.StateAccessProblem(); blocked != nil {
		t.Fatalf("blocked = %+v", blocked)
	}
}

// The launch-time warning and the doctor share one verdict: the warning
// carries exactly the error the doctor is built from.
func TestLaunchWarningAndDoctorShareTheVerdict(t *testing.T) {
	d, blocker := stateUnderPrivateParent(t)
	account := runtimeAccount(t)
	actAsRootWithAccount(t, account, nil)
	d.runUser = dedicatedRunUser
	d.runUID, _ = strconv.Atoi(account.Uid)
	d.runGID, _ = strconv.Atoi(account.Gid)

	var logs bytes.Buffer
	saved := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(saved) })

	d.warnUntraversableAncestors()
	_, blocked := d.StateAccessProblem()
	if blocked == nil {
		t.Fatal("doctor verdict missing")
	}
	if !strings.Contains(logs.String(), "runtime user cannot reach the agent state directory") || !strings.Contains(logs.String(), blocker+" is not traversable") {
		t.Fatalf("warning = %s", logs.String())
	}
	if !strings.Contains(logs.String(), strings.ReplaceAll(blocked.Error(), `"`, `\"`)) {
		t.Fatalf("warning does not carry the doctor's verdict %q:\n%s", blocked.Error(), logs.String())
	}
}
