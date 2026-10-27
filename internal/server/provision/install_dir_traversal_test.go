package provision

import (
	"path/filepath"
	"strings"
	"testing"
)

const localDiskMounts = "/dev/sda1 %DIR% ext4 rw,relatime 0 0"

// A root agent runs workloads as the udocker user; an install directory under
// a private ancestor (/root, a 0700 home) is invisible to it, and every launch
// then fails with `fork/exec .../proot-...: permission denied`.
func TestInstallDirTraversalBlockedByPrivateAncestor(t *testing.T) {
	dir := realTempDir(t)
	private := filepath.Dir(dir)
	out := probeScenario(t, dirScenario{
		mounts: localDiskMounts, runtimeID: "1001",
		modes: map[string]string{private: "700 0 0"},
	}, dir, dir, "vm")
	wantCheck(t, out.Checks, checkInstallDirTraversal, "error",
		private+" (mode 700, owner 0:0)", "udocker, uid 1001", "permission denied",
		"chmod o+x "+private, "choose an install directory outside it, such as /opt/gpuworks")
	// The other verdicts are unaffected and the install is not stopped.
	wantCheck(t, out.Checks, checkInstallDirOwnership, "ok")
	if out.Noexec {
		t.Fatal("a closed ancestor must not stop the install")
	}
}

func TestInstallDirTraversalQuotesAnAwkwardDirectoryInTheFix(t *testing.T) {
	c := installDirTraversalCheck(installDirFacts{
		requested: "/home/my user/gpuworks", ancestors: "blocked", ancestorDir: "/home/my user",
		ancestorMode: "700", ancestorUID: "1000", ancestorGID: "1000", runtimeUID: "1001",
	})
	if c.Status != "error" || !strings.Contains(c.Detail, "`chmod o+x '/home/my user'`") {
		t.Fatalf("check = %+v", c)
	}
}

func TestInstallDirTraversalWarnsWhereDockerKeepsTheImages(t *testing.T) {
	dir := realTempDir(t)
	out := probeScenario(t, dirScenario{
		mounts: localDiskMounts, runtimeID: "1001", docker: true,
		modes: map[string]string{filepath.Dir(dir): "700 0 0"},
	}, dir, dir, "vm")
	wantCheck(t, out.Checks, checkInstallDirTraversal, "warn", "chmod o+x "+filepath.Dir(dir), "this node has docker")
}

// Search for the runtime user comes from exactly one class, the way the
// kernel picks it: its uid, else its primary group, else others.
func TestInstallDirTraversalPicksTheClassTheKernelPicks(t *testing.T) {
	for _, tc := range []struct {
		name      string
		runtimeID string
		mode      string
		blocked   bool
	}{
		{"others have search", "1001", "701 0 0", false},
		{"others have only read and write", "1001", "706 0 0", true},
		{"group is the runtime user's and has search", "1001", "710 0 1001", false},
		{"group is the runtime user's and lacks search", "1001", "760 0 1001", true},
		{"another group has search, others do not", "1001", "710 0 0", true},
		{"owner class applies to the runtime user alone", "1001", "100 1001 1001", false},
		{"owner class blocks even where others would pass", "1001", "007 1001 1001", true},
		{"sticky world-writable /tmp", "1001", "1777 0 0", false},
		{"no account yet: only others count, closed", "", "750 0 0", true},
		{"no account yet: only others count, open", "", "751 0 0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := realTempDir(t)
			out := probeScenario(t, dirScenario{
				mounts: localDiskMounts, runtimeID: tc.runtimeID,
				modes: map[string]string{filepath.Dir(dir): tc.mode},
			}, dir, dir, "vm")
			if tc.blocked {
				wantCheck(t, out.Checks, checkInstallDirTraversal, "error", "mode "+strings.Fields(tc.mode)[0])
			} else {
				wantCheck(t, out.Checks, checkInstallDirTraversal, "ok", "can be searched")
			}
		})
	}
}

func TestInstallDirTraversalBeforeTheUdockerAccountExists(t *testing.T) {
	dir := realTempDir(t)
	out := probeScenario(t, dirScenario{
		mounts: localDiskMounts, modes: map[string]string{filepath.Dir(dir): "750 0 0"},
	}, dir, dir, "vm")
	wantCheck(t, out.Checks, checkInstallDirTraversal, "error", "udocker, created at install")
}

// The walk goes to the top: the blocking directory is named wherever it is,
// and the nearest closed one is the one reported.
func TestInstallDirTraversalNamesTheNearestClosedAncestor(t *testing.T) {
	dir := realTempDir(t)
	parent := filepath.Dir(dir)
	grand := filepath.Dir(parent)
	out := probeScenario(t, dirScenario{
		mounts: localDiskMounts, runtimeID: "1001",
		modes: map[string]string{grand: "700 0 0", parent: "750 0 0"},
	}, dir, dir, "vm")
	wantCheck(t, out.Checks, checkInstallDirTraversal, "error", parent+" (mode 750")
	if strings.Contains(checkByName(t, out.Checks, checkInstallDirTraversal).Detail, grand+" (mode") {
		t.Fatalf("reported the farther directory: %+v", out.Checks)
	}
	out = probeScenario(t, dirScenario{
		mounts: localDiskMounts, runtimeID: "1001",
		modes: map[string]string{grand: "700 0 0", parent: "755 0 0"},
	}, dir, dir, "vm")
	wantCheck(t, out.Checks, checkInstallDirTraversal, "error", grand+" (mode 700")
}

// The install directory itself is the installer's: it opens it (0711), so its
// own mode says nothing.
func TestInstallDirTraversalDoesNotJudgeTheInstallDirectoryItself(t *testing.T) {
	dir := realTempDir(t)
	out := probeScenario(t, dirScenario{
		mounts: localDiskMounts, runtimeID: "1001", modes: map[string]string{dir: "700 0 0"},
	}, dir, dir, "vm")
	wantCheck(t, out.Checks, checkInstallDirTraversal, "ok", "can be searched")
}

// A directory that does not exist yet is created under the nearest existing
// one, which is then the first that must be open.
func TestInstallDirTraversalOfADirectoryNotCreatedYet(t *testing.T) {
	parent := realTempDir(t)
	requested := filepath.Join(parent, "models", "gpuworks")
	out := probeScenario(t, dirScenario{
		mounts: localDiskMounts, runtimeID: "1001", modes: map[string]string{parent: "700 0 0"},
	}, parent, requested, "vm")
	wantCheck(t, out.Checks, checkInstallDirTraversal, "error", parent+" (mode 700", "chmod o+x "+parent)
}

// Without root or passwordless sudo the agent runs as the SSH user and so do
// its workloads: nothing to grant, nothing to judge.
func TestInstallDirTraversalNotNeededWithoutRoot(t *testing.T) {
	dir := realTempDir(t)
	out := probeScenario(t, dirScenario{
		uid: "1000", user: "ubuntu", mounts: localDiskMounts, modes: map[string]string{filepath.Dir(dir): "700 0 0"},
	}, dir, dir, "vm")
	wantCheck(t, out.Checks, checkInstallDirTraversal, "ok", "not needed", "ubuntu is not root")
}

// Passwordless sudo makes the agent root, and stat runs with its privileges
// because the SSH user may be unable to look inside a private home.
func TestInstallDirTraversalBlockedForANonRootUserWithSudo(t *testing.T) {
	dir := realTempDir(t)
	out := probeScenario(t, dirScenario{
		uid: "1000", user: "ubuntu", sudo: true, mounts: localDiskMounts, runtimeID: "1001",
		modes: map[string]string{filepath.Dir(dir): "700 0 0"},
	}, dir, dir, "vm")
	wantCheck(t, out.Checks, checkInstallDirTraversal, "error", "chmod o+x "+filepath.Dir(dir))
}

// A stat that fails says nothing about the node: reported as not checked, not
// as a verdict.
func TestInstallDirTraversalUnreadableAncestorIsNotAVerdict(t *testing.T) {
	dir := realTempDir(t)
	out := probeScenario(t, dirScenario{
		mounts: localDiskMounts, runtimeID: "1001", modes: map[string]string{filepath.Dir(dir): "unreadable"},
	}, dir, dir, "vm")
	wantCheck(t, out.Checks, checkInstallDirTraversal, "ok", "not checked", "could not be read")
}

func TestInstallDirTraversalNotNeededOnAMac(t *testing.T) {
	c := installDirTraversalCheck(installDirFacts{mac: true, ancestors: "blocked", ancestorDir: "/Users/x"})
	if c.Status != "ok" || !strings.Contains(c.Detail, "not needed on a Mac") {
		t.Fatalf("check = %+v", c)
	}
}

func TestParseInstallDirFactsReadsTheAncestorVerdict(t *testing.T) {
	f, err := parseInstallDirFacts("probed=/root\nchown=ok\nruntime_uid=1001\nancestors=blocked\nancestor_dir=/root\nancestor_mode=700\nancestor_uid=0\nancestor_gid=0\n")
	if err != nil {
		t.Fatal(err)
	}
	if f.ancestors != "blocked" || f.ancestorDir != "/root" || f.ancestorMode != "700" || f.ancestorUID != "0" || f.ancestorGID != "0" || f.runtimeUID != "1001" {
		t.Fatalf("facts = %+v", f)
	}
}
