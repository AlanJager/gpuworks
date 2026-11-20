package engine

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func managedDirSpec(uuid string, deploymentID uint, group string, names ...string) proto.InstanceDesiredState {
	spec := proto.InstanceDesiredState{UUID: uuid, DeploymentID: deploymentID, Engine: "callable", DesiredState: "running", APIPort: 30000}
	for _, name := range names {
		spec.Mounts = append(spec.Mounts, proto.MountSpec{
			Target: "/" + name, Purpose: proto.MountPurposeManagedDir, SizeHint: "1073741824",
			ManagedDir: &proto.ManagedDirRef{Group: group, Name: name},
		})
	}
	return spec
}

func newManagedDirEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	state := t.TempDir()
	root := filepath.Join(state, "runtime-mounts")
	t.Setenv(runtimeMountRootEnv, root)
	return NewWithProber(nil, func(proto.InstanceStatus) {}, slog.New(slog.DiscardHandler), state, injectionProber{}), root
}

// The server names the directory; the node creates it under the deployment's
// group, writes the host path into the workload, and the instance that
// replaces this one (a group re-formed on the node) finds what was written.
func TestEngineResolvesManagedDirMountsToPersistentHostDirectories(t *testing.T) {
	e, root := newManagedDirEngine(t)
	spec := managedDirSpec("i-1", 7, "7-1700000000", "pico-state", "logs")
	ws := toWorkloadSpec(spec, "container-i-1")
	if err := e.applyManagedDirMounts(&ws, spec); err != nil {
		t.Fatal(err)
	}
	state, found := mountFor(ws, "/pico-state")
	want := filepath.Join(root, proto.MountPurposeManagedDir, "7-1700000000", "pico-state")
	if !found || state.Source != want || state.ReadOnly {
		t.Fatalf("mount = %#v (found %v), want a writable %s", state, found, want)
	}
	if logs, _ := mountFor(ws, "/logs"); logs.Source != filepath.Join(root, proto.MountPurposeManagedDir, "7-1700000000", "logs") {
		t.Fatalf("logs mount = %#v", logs)
	}
	if err := os.WriteFile(filepath.Join(want, "session.db"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}

	replacement := managedDirSpec("i-2", 7, "7-1700000000", "pico-state")
	ws = toWorkloadSpec(replacement, "container-i-2")
	if err := e.applyManagedDirMounts(&ws, replacement); err != nil {
		t.Fatal(err)
	}
	if mount, _ := mountFor(ws, "/pico-state"); mount.Source != want {
		t.Fatalf("replacement mount = %#v, want %s", mount, want)
	}
	if data, err := os.ReadFile(filepath.Join(want, "session.db")); err != nil || string(data) != "kept" {
		t.Fatalf("the replacement lost the directory's content: %q %v", data, err)
	}
	if groups := e.ManagedDirGroups(); len(groups) != 1 || groups[0] != "7-1700000000" {
		t.Fatalf("groups = %v", groups)
	}
	if e.ManagedDirRoot() != filepath.Join(root, proto.MountPurposeManagedDir) {
		t.Fatalf("root = %s", e.ManagedDirRoot())
	}
}

// A mount that does not say which directory it is, or names one with a path
// in it, is refused before anything is created: the node never guesses whose
// directory a mount is or writes outside the managed root.
func TestEngineRefusesManagedDirMountsItCannotPlace(t *testing.T) {
	for name, mount := range map[string]proto.MountSpec{
		"no reference":    {Target: "/state", Purpose: proto.MountPurposeManagedDir},
		"no group":        {Target: "/state", Purpose: proto.MountPurposeManagedDir, ManagedDir: &proto.ManagedDirRef{Name: "state"}},
		"group traversal": {Target: "/state", Purpose: proto.MountPurposeManagedDir, ManagedDir: &proto.ManagedDirRef{Group: "../7-1", Name: "state"}},
		"name traversal":  {Target: "/state", Purpose: proto.MountPurposeManagedDir, ManagedDir: &proto.ManagedDirRef{Group: "7-1", Name: "../../etc"}},
	} {
		t.Run(name, func(t *testing.T) {
			e, root := newManagedDirEngine(t)
			spec := proto.InstanceDesiredState{UUID: "i-1", DeploymentID: 7}
			spec.Mounts = []proto.MountSpec{mount}
			ws := toWorkloadSpec(spec, "container")
			if err := e.applyManagedDirMounts(&ws, spec); err == nil {
				t.Fatal("an unplaceable managed directory mount was accepted")
			}
			if _, err := os.Stat(filepath.Join(root, proto.MountPurposeManagedDir)); !os.IsNotExist(err) {
				t.Fatalf("a managed directory root was created for a refused mount: %v", err)
			}
		})
	}
}

// Other mounts, and a managed mount whose source is already stated, are left
// as the server sent them.
func TestEngineLeavesNonManagedMountsAlone(t *testing.T) {
	e, root := newManagedDirEngine(t)
	spec := proto.InstanceDesiredState{UUID: "i-1", DeploymentID: 7}
	spec.Mounts = []proto.MountSpec{
		{Source: "/srv/data", Target: "/data"},
		{Source: "/already", Target: "/state", Purpose: proto.MountPurposeManagedDir, ManagedDir: &proto.ManagedDirRef{Group: "7-1", Name: "state"}},
	}
	ws := toWorkloadSpec(spec, "container")
	if err := e.applyManagedDirMounts(&ws, spec); err != nil {
		t.Fatal(err)
	}
	if mount, _ := mountFor(ws, "/data"); mount.Source != "/srv/data" {
		t.Fatalf("mount = %#v", mount)
	}
	if mount, _ := mountFor(ws, "/state"); mount.Source != "/already" {
		t.Fatalf("mount = %#v", mount)
	}
	if _, err := os.Stat(filepath.Join(root, proto.MountPurposeManagedDir)); !os.IsNotExist(err) {
		t.Fatalf("a managed directory was created: %v", err)
	}
}

// Releasing by the replay's group list removes a released group nothing here
// mounts, keeps one a desired instance still mounts, and never touches an
// entry that is not named like a group.
func TestEngineReleasesManagedDirGroupsTheServerNames(t *testing.T) {
	e, root := newManagedDirEngine(t)
	for _, spec := range []proto.InstanceDesiredState{
		managedDirSpec("i-1", 7, "7-100", "state"),
		managedDirSpec("i-2", 8, "8-200", "state"),
		managedDirSpec("i-3", 9, "9-300", "state"),
	} {
		ws := toWorkloadSpec(spec, "container-"+spec.UUID)
		if err := e.applyManagedDirMounts(&ws, spec); err != nil {
			t.Fatal(err)
		}
	}
	foreign := filepath.Join(root, proto.MountPurposeManagedDir, "not-a-group")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	if groups := e.ManagedDirGroups(); strings.Join(groups, ",") != "7-100,8-200,9-300" {
		t.Fatalf("groups = %v, want only the entries named like a group", groups)
	}
	still := managedDirSpec("i-2", 8, "8-200", "state")
	e.mu.Lock()
	e.desired[still.UUID] = still
	e.mu.Unlock()

	e.ReleaseManagedDirGroups([]string{"7-100", "8-200", "../not-a-group", "not-a-group"})
	group := func(key string) string { return filepath.Join(root, proto.MountPurposeManagedDir, key) }
	if _, err := os.Stat(group("7-100")); !os.IsNotExist(err) {
		t.Fatalf("released group 7-100 survived: %v", err)
	}
	if _, err := os.Stat(group("8-200")); err != nil {
		t.Fatalf("a group a desired instance mounts was removed: %v", err)
	}
	if _, err := os.Stat(group("9-300")); err != nil {
		t.Fatalf("a group the server did not name was removed: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("an entry not named like a group was removed: %v", err)
	}
}

// A deleted deployment's update removes every group of that deployment id -
// including one an earlier deployment of the same id left - and nothing of any
// other deployment.
func TestEngineReleasesEveryGroupOfADeletedDeployment(t *testing.T) {
	e, _ := newManagedDirEngine(t)
	for _, spec := range []proto.InstanceDesiredState{
		managedDirSpec("i-1", 7, "7-100", "state"),
		managedDirSpec("i-2", 7, "7-50", "state"),
		managedDirSpec("i-3", 70, "70-100", "state"),
	} {
		ws := toWorkloadSpec(spec, "container-"+spec.UUID)
		if err := e.applyManagedDirMounts(&ws, spec); err != nil {
			t.Fatal(err)
		}
	}
	e.ReleaseManagedDirDeployments(nil)
	e.ReleaseManagedDirDeployments([]uint{7})
	if groups := e.ManagedDirGroups(); strings.Join(groups, ",") != "70-100" {
		t.Fatalf("groups after release = %v, want only deployment 70's", groups)
	}
}

// A node that never held a managed directory lists none and releases nothing.
func TestEngineWithoutManagedDirsListsNoGroups(t *testing.T) {
	e, _ := newManagedDirEngine(t)
	if groups := e.ManagedDirGroups(); groups != nil {
		t.Fatalf("groups = %v", groups)
	}
	e.ReleaseManagedDirGroups([]string{"7-100"})
	e.ReleaseManagedDirDeployments([]uint{7})
}
