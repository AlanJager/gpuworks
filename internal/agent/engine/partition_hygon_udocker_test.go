package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/assets"
	"gpuworks.ai/gpuworks/internal/proto"
)

func hygonMountAt(t *testing.T, ws *agentruntime.WorkloadSpec, target string) agentruntime.Mount {
	t.Helper()
	var found []agentruntime.Mount
	for _, mount := range ws.Mounts {
		if mount.Target == target {
			found = append(found, mount)
		}
	}
	if len(found) != 1 {
		t.Fatalf("mount %s = %+v, want one: %+v", target, found, ws.Mounts)
	}
	return found[0]
}

func assertHygonUdockerView(t *testing.T, ws *agentruntime.WorkloadSpec, instanceDir string, id int) {
	t.Helper()
	view := hygonMountAt(t, ws, "/etc/vdev")
	logs := hygonMountAt(t, ws, "/var/log/hylog")
	if filepath.Dir(view.Source) != instanceDir || filepath.Dir(logs.Source) != instanceDir {
		t.Fatalf("view/logs outside instance directory: %+v / %+v", view, logs)
	}
	if view.Source == instanceDir || logs.Source == view.Source {
		t.Fatalf("ledger directory or shared source exposed: %+v / %+v", view, logs)
	}
	var paths []string
	if err := filepath.WalkDir(view.Source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != view.Source {
			rel, err := filepath.Rel(view.Source, path)
			if err != nil {
				return err
			}
			paths = append(paths, filepath.ToSlash(rel))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	name := fmt.Sprintf("vdev%d.conf", id)
	want := []string{"docker", "docker/" + name, name}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("mounted config view = %v, want only own native filename %v", paths, want)
	}
	snapshot, err := os.ReadFile(filepath.Join(instanceDir, name))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{filepath.Join(view.Source, name), filepath.Join(view.Source, "docker", name)} {
		data, err := os.ReadFile(file)
		if err != nil || !bytes.Equal(data, snapshot) {
			t.Fatalf("view differs from verified snapshot: %s/%v", file, err)
		}
		if info, err := os.Stat(file); err != nil || info.Mode().Perm()&0o004 == 0 {
			t.Fatalf("runtime user cannot read config: %v/%v", info, err)
		}
	}
	if info, err := os.Stat(logs.Source); err != nil || !info.IsDir() || info.Mode().Perm() != 0o777 || info.Mode()&os.ModeSticky == 0 {
		t.Fatalf("runtime needs an independent writable sticky log directory: %v/%v", info, err)
	}
	for _, mount := range ws.Mounts {
		if strings.HasPrefix(mount.Target, "/etc/vdev/") {
			t.Fatalf("individual file binding may expose foreign host configs: %+v", mount)
		}
	}
	for _, arg := range ws.DockerArgs {
		if arg == "--tmpfs" || strings.Contains(arg, "/var/log/hylog") {
			t.Fatalf("udocker cannot prepare logs through Docker args: %v", ws.DockerArgs)
		}
	}
}

func TestHygonUdockerUsesIndependentViewsLogsAndOwnedCleanup(t *testing.T) {
	e, h, fake := hygonTestEngine(t)
	e.driver = hygonTestDriver{name: "udocker"}
	apply := func(uuid string) *agentruntime.WorkloadSpec {
		t.Helper()
		ws := &agentruntime.WorkloadSpec{Image: "hygon/inference:dtk26"}
		if ok, reason := h.apply(e, t.Context(), ws, proto.InstanceDesiredState{UUID: uuid, GPUIndexes: []int{0}, VRAMLimitBytes: 1536 << 20}); !ok {
			t.Fatal(reason)
		}
		return ws
	}
	first, second := apply("first"), apply("second")
	assertHygonUdockerView(t, first, hygonInstanceDir(e, "first"), 4)
	assertHygonUdockerView(t, second, hygonInstanceDir(e, "second"), 5)
	if len(first.Mounts) != 5 || len(second.Mounts) != 5 || hygonMountAt(t, first, "/etc/vdev").Source == hygonMountAt(t, second, "/etc/vdev").Source || hygonMountAt(t, first, "/var/log/hylog").Source == hygonMountAt(t, second, "/var/log/hylog").Source {
		t.Fatalf("views/logs shared or runtime bindings missing: %+v / %+v", first.Mounts, second.Mounts)
	}
	for _, ws := range []*agentruntime.WorkloadSpec{first, second} {
		if got := hygonMountAt(t, ws, hygonVirtualLock); got.Source != h.lockPath {
			t.Fatalf("not using the real shared vendor lock: %+v", got)
		}
		if hygonMountAt(t, ws, "/opt/hygondriver").Source != h.dtkDir || hygonMountAt(t, ws, "/opt/hyhal").Source != h.hyhalDir {
			t.Fatalf("driver paths changed: %+v", ws.Mounts)
		}
	}
	log := filepath.Join(hygonMountAt(t, first, "/var/log/hylog").Source, "existing.log")
	if err := os.WriteFile(log, []byte("keep on restart"), 0o600); err != nil {
		t.Fatal(err)
	}
	retry := apply("first")
	if !reflect.DeepEqual(first.Mounts, retry.Mounts) || fake.nextID != 6 {
		t.Fatalf("restart changed mounts or allocated another native device: %v/%v next=%d", first.Mounts, retry.Mounts, fake.nextID)
	}
	if data, err := os.ReadFile(log); err != nil || string(data) != "keep on restart" {
		t.Fatalf("restart replaced log data: %q/%v", data, err)
	}
	fake.releaseErr = errors.New("native destroy refused")
	if err := h.cleanupInstance(e, "first"); err == nil || !strings.Contains(err.Error(), "native destroy refused") {
		t.Fatalf("cleanup failure swallowed: %v", err)
	}
	assertHygonUdockerView(t, first, hygonInstanceDir(e, "first"), 4)
	fake.releaseErr = nil
	if err := h.cleanupInstance(e, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hygonInstanceDir(e, "first")); !os.IsNotExist(err) || !fake.owned[5] {
		t.Fatalf("first state remains or survivor released: %v/%v", err, fake.owned)
	}
	assertHygonUdockerView(t, second, hygonInstanceDir(e, "second"), 5)
}

func TestHygonUdockerProbeHasDTKAndPrivateWritableLogs(t *testing.T) {
	e, h, _ := hygonTestEngine(t)
	e.driver = hygonTestDriver{name: "udocker"}
	ws := &agentruntime.WorkloadSpec{Image: assets.PartitionProbeImage}
	if ok, reason := h.apply(e, t.Context(), ws, proto.InstanceDesiredState{UUID: "probe-udocker", GPUIndexes: []int{0}, VRAMLimitBytes: 4096 << 20}); !ok {
		t.Fatal(reason)
	}
	assertHygonUdockerView(t, ws, hygonInstanceDir(e, "probe-udocker"), 4)
	if got := hygonMountAt(t, ws, "/opt/dtk"); got.Source != h.dtkDir || !got.ReadOnly {
		t.Fatalf("probe toolkit = %+v", got)
	}
}

type hygonCreateHook struct {
	*fakeHygonVdevHelper
	after func(string) error
}

func (h hygonCreateHook) Create(ctx context.Context, index, cus, memoryMiB int, recordPath, pci string) (int, error) {
	id, err := h.fakeHygonVdevHelper.Create(ctx, index, cus, memoryMiB, recordPath, pci)
	if err == nil {
		err = h.after(filepath.Dir(recordPath))
	}
	return id, err
}

func TestHygonUdockerDirectoryFailuresPreserveOwnedLedgerForRetryAndCleanup(t *testing.T) {
	for _, blocked := range []string{"vdev-view", "hylog"} {
		t.Run(blocked, func(t *testing.T) {
			e, h, fake := hygonTestEngine(t)
			e.driver = hygonTestDriver{name: "udocker"}
			h.helper = hygonCreateHook{fakeHygonVdevHelper: fake, after: func(dir string) error {
				return os.WriteFile(filepath.Join(dir, blocked), []byte("directory blocked"), 0o644)
			}}
			spec := proto.InstanceDesiredState{UUID: "failed-view", GPUIndexes: []int{0}, VRAMLimitBytes: 1536 << 20}
			if ok, reason := h.apply(e, t.Context(), &agentruntime.WorkloadSpec{}, spec); ok || !strings.Contains(reason, "view") {
				t.Fatalf("directory failure: %v/%s", ok, reason)
			}
			dir := hygonInstanceDir(e, spec.UUID)
			if _, found, err := readHygonOwnedVdev(dir); err != nil || !found || !fake.owned[4] {
				t.Fatalf("allocated vdev lost its recovery ledger: %v/%v/%v", found, err, fake.owned)
			}
			if err := os.Remove(filepath.Join(dir, blocked)); err != nil {
				t.Fatal(err)
			}
			ws := &agentruntime.WorkloadSpec{}
			if ok, reason := h.apply(e, t.Context(), ws, spec); !ok {
				t.Fatal(reason)
			}
			if fake.nextID != 5 {
				t.Fatalf("retry created another vdev: %d", fake.nextID)
			}
			assertHygonUdockerView(t, ws, dir, 4)
			if err := h.cleanupInstance(e, spec.UUID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHygonUdockerRejectsForeignAndSymlinkedViewEntries(t *testing.T) {
	for _, change := range []string{"foreign-config", "legacy-alias", "changed-config", "symlink-view", "symlink-log"} {
		t.Run(change, func(t *testing.T) {
			e, h, fake := hygonTestEngine(t)
			e.driver = hygonTestDriver{name: "udocker"}
			spec := proto.InstanceDesiredState{UUID: "protected-view", GPUIndexes: []int{0}, VRAMLimitBytes: 1536 << 20}
			ws := &agentruntime.WorkloadSpec{}
			if ok, reason := h.apply(e, t.Context(), ws, spec); !ok {
				t.Fatal(reason)
			}
			view := hygonMountAt(t, ws, "/etc/vdev").Source
			logs := hygonMountAt(t, ws, "/var/log/hylog").Source
			var err error
			switch change {
			case "foreign-config":
				err = os.WriteFile(filepath.Join(view, "vdev8.conf"), []byte("foreign"), 0o644)
			case "legacy-alias":
				err = os.WriteFile(filepath.Join(view, "docker", "vdev0.conf"), []byte("fixed-ID alias"), 0o644)
			case "changed-config":
				err = os.WriteFile(filepath.Join(view, "docker", "vdev4.conf"), []byte("changed"), 0o644)
			case "symlink-view", "symlink-log":
				path := view
				if change == "symlink-log" {
					path = logs
				}
				if err = os.RemoveAll(path); err == nil {
					err = os.Symlink(t.TempDir(), path)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if ok, reason := h.apply(e, t.Context(), &agentruntime.WorkloadSpec{}, spec); ok || !strings.Contains(reason, "view") {
				t.Fatalf("unsafe view accepted: %v/%s", ok, reason)
			}
			if fake.nextID != 5 || !fake.owned[4] {
				t.Fatalf("rejected view changed native allocation: %d/%v", fake.nextID, fake.owned)
			}
		})
	}
}
