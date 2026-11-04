package selfinstall

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Selecting the historical application root for installation must not inherit
// the extra restriction on deleting that root during uninstall, but the root
// itself is never handed to the service account.
func TestPermissionRootsAcceptHistoricalApplicationRoot(t *testing.T) {
	shared, err := resolveUninstallPath("/opt/gpuworks")
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, t.TempDir())
	h.installer.layout = NewLayout("/opt/gpuworks", "", "")
	roots, err := h.installer.permissionRoots()
	if err != nil {
		t.Fatalf("historical install root rejected: %v", err)
	}
	if slices.Contains(roots, shared) {
		t.Fatalf("permission roots = %v, must not include the shared root %s", roots, shared)
	}
	for _, role := range []string{"data", "logs"} {
		t.Run(role, func(t *testing.T) {
			h := newHarness(t, t.TempDir())
			if role == "data" {
				h.installer.layout.DataDir = "/opt/gpuworks"
			} else {
				h.installer.layout.LogsDir = "/opt/gpuworks"
			}
			if _, err := h.installer.permissionRoots(); err == nil ||
				!strings.Contains(err.Error(), "refusing recursive permission changes for /opt/gpuworks") {
				t.Fatalf("shared root as %s directory: err = %v, want a refusal", role, err)
			}
		})
	}
	if err := safeUninstallDirectory("/opt/gpuworks"); err == nil {
		t.Fatal("install compatibility must not allow deleting the shared agent/server root")
	}
}

// withSharedRoot points the shared application root at a temporary tree that
// holds the colocated agent's root-owned state.
func withSharedRoot(t *testing.T) (string, []string) {
	t.Helper()
	root := mustEvalSymlinks(t, t.TempDir())
	previous := sharedApplicationRoot
	sharedApplicationRoot = root
	t.Cleanup(func() { sharedApplicationRoot = previous })
	var agent []string
	for _, rel := range []string{"bin/gpuworks-hygon-vdev-helper", "partition/libvruntime.so",
		"probe-libs/libprobe.so", "runveil/libascend-compat.so", "models/tiny/config.json", "identity.json"} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, "agent state", 0o755)
		agent = append(agent, path)
	}
	return root, agent
}

// A server installed into the agent's state directory shares only its own
// entries with the service account: a recursive chown of the directory would
// hand the account binaries and libraries the root agent runs.
func TestInstallIntoSharedRootLeavesAgentStateWithRoot(t *testing.T) {
	root, agent := withSharedRoot(t)
	h := newHarness(t, root)
	h.writeExistingInstall("v1.0.0")
	h.installSelf("replacement binary")
	req := h.quiet()
	req.ServiceUser = DefaultServiceUser
	h.mustInstall(req)

	var got []string
	for _, cmd := range h.run.calls {
		if cmd.Path != "chown" && cmd.Path != "chmod" && cmd.Path != "find" {
			continue
		}
		target := cmd.Args[len(cmd.Args)-1]
		if cmd.Path == "find" {
			target = cmd.Args[0]
		}
		if target == root {
			t.Errorf("%s changed the shared root itself: %s", cmd.Path, cmd)
		}
		for _, path := range agent {
			if pathWithin(target, path) {
				t.Errorf("%s reaches agent state %s: %s", cmd.Path, path, cmd)
			}
		}
		if cmd.Path == "chown" {
			got = append(got, target)
		}
	}
	for _, want := range []string{h.layout.Binary(), h.layout.ConfigPath(), h.layout.DataDir,
		h.layout.LogsDir, h.layout.TmpDir(), h.layout.BackupsDir} {
		if !slices.Contains(got, want) {
			t.Errorf("chown targets = %v, missing server entry %s", got, want)
		}
	}
	if !strings.Contains(h.errOut.String(), "shared with the node agent") {
		t.Errorf("install did not warn that console upgrades need the CLI: %q", h.errOut.String())
	}
}

// An entry of the shared root that is a symlink could aim the recursive change
// at the agent's files beside it.
func TestPermissionRootsRefuseSymlinkedEntryInSharedRoot(t *testing.T) {
	root, _ := withSharedRoot(t)
	h := newHarness(t, root)
	h.writeExistingInstall("v1.0.0")
	if err := os.Symlink(filepath.Join(root, "partition"), h.layout.TmpDir()); err != nil {
		t.Fatal(err)
	}
	if err := h.installer.chownTree(context.Background(), "gpuworks"); err == nil ||
		!strings.Contains(err.Error(), "is a symlink to") {
		t.Fatalf("err = %v, want the symlinked entry refused", err)
	}
	if h.run.ran("sudo chown") {
		t.Fatal("chown ran before the symlinked entry was refused")
	}
}

func TestInstallPermissionRejectionPreservesExistingInstallation(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.installSelf("replacement binary")
	// The filesystem is valid, but the installation root is deliberately a
	// protected HOME identity. A deterministic policy rejection must precede
	// binary/config/unit writes or stopping an existing service.
	t.Setenv("HOME", h.layout.Dir)
	paths := []string{h.layout.Binary(), h.layout.ConfigPath(), h.layout.UnitPath(),
		filepath.Join(h.layout.DataDir, "gpuworks.db"), filepath.Join(h.layout.DataDir, "master.key")}
	before := map[string][]byte{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = data
	}
	req := h.quiet()
	req.ServiceUser = DefaultServiceUser
	if _, err := h.installer.Install(context.Background(), req); err == nil ||
		!strings.Contains(err.Error(), "refusing recursive permission changes for "+h.layout.Dir) {
		t.Fatalf("want permission-policy rejection, got %v", err)
	}
	for _, path := range paths {
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(before[path], after) {
			t.Errorf("installation was modified before permission rejection: %s", path)
		}
	}
	for _, action := range []string{"stop", "restart", "start", "daemon-reload"} {
		if h.run.ran("sudo systemctl " + action) {
			t.Errorf("service action %s ran before permission rejection", action)
		}
	}
}

func TestInstallPermissionRejectionCreatesNoPartialInstallation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fresh")
	h := newHarness(t, dir)
	h.installSelf("new binary")
	t.Setenv("HOME", h.layout.Dir)
	req := h.quiet()
	req.ServiceUser = DefaultServiceUser
	req.Password = "permission-regression-password-2026"
	if _, err := h.installer.Install(context.Background(), req); err == nil ||
		!strings.Contains(err.Error(), "refusing recursive permission changes") {
		t.Fatalf("want permission-policy rejection, got %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("rejected install created its directory: %v", err)
	}
}
