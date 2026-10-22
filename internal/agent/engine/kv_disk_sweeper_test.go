package engine

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// writeBlock lays down one file of the given size with a controlled mtime, so a
// test can state the write order the sweeper prunes by.
func writeBlock(t *testing.T, dir, name string, size int, age time.Duration) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

func remaining(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	if err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, relErr := filepath.Rel(root, name)
		if relErr != nil {
			return relErr
		}
		names = append(names, relative)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	return names
}

func testSweeper(t *testing.T, targets []KVDiskTarget) (*KVDiskSweeper, *[]proto.MetricSample) {
	t.Helper()
	var reported []proto.MetricSample
	sweeper := NewKVDiskSweeper("node-1", func() []KVDiskTarget { return targets },
		func(samples []proto.MetricSample) { reported = append(reported, samples...) },
		slog.New(slog.DiscardHandler))
	sweeper.now = func() time.Time { return time.UnixMilli(1700000000000) }
	return sweeper, &reported
}

// Under the limit nothing is touched, and the usage the panel shows is the sum
// of the block files rather than of everything in the directory.
func TestKVDiskSweeperLeavesADirectoryUnderItsLimit(t *testing.T) {
	dir := t.TempDir()
	writeBlock(t, dir, "model_abc/config.json", 100, time.Hour)
	writeBlock(t, dir, "model_abc_r0/aaa/bb_g0/one.bin", 400, 2*time.Hour)
	writeBlock(t, dir, "model_abc_r0/aaa/bb_g0/two.bin_123.tmp", 4000, 3*time.Hour)

	sweeper, reported := testSweeper(t, []KVDiskTarget{{
		DeploymentID: 7, Instances: []string{"i-1"}, Engine: "vllm", Dir: dir, LimitBytes: 1000,
	}})
	sweeper.RunOnce()

	want := []string{"model_abc/config.json", "model_abc_r0/aaa/bb_g0/one.bin", "model_abc_r0/aaa/bb_g0/two.bin_123.tmp"}
	if got := remaining(t, dir); len(got) != len(want) {
		t.Fatalf("files = %v, want all of %v kept", got, want)
	}
	assertSample(t, *reported, "gpuworks_kv_disk_used_bytes", "i-1", 400)
	assertSample(t, *reported, "gpuworks_kv_disk_limit_bytes", "i-1", 1000)
}

// Over the limit the oldest blocks go until the directory is back under the
// hysteresis target, and only block files are ever candidates: removing the
// tier's config.json would make the engine rebuild its layout, and removing a
// .tmp would break a write that is about to be renamed into place.
func TestKVDiskSweeperPrunesOldestBlocksToTheHysteresisTarget(t *testing.T) {
	dir := t.TempDir()
	writeBlock(t, dir, "model_abc/config.json", 100, 10*time.Hour)
	writeBlock(t, dir, "r0/pending.bin_9.tmp", 100, 9*time.Hour)
	writeBlock(t, dir, "r0/oldest.bin", 300, 8*time.Hour)
	writeBlock(t, dir, "r0/middle.bin", 300, 5*time.Hour)
	writeBlock(t, dir, "r0/newest.bin", 300, time.Minute)

	// 900 bytes of blocks against a 500 byte limit: the target is 450, so the
	// two oldest blocks have to go and the newest must survive.
	sweeper, reported := testSweeper(t, []KVDiskTarget{{
		DeploymentID: 7, Instances: []string{"i-1"}, Engine: "vllm", Dir: dir, LimitBytes: 500,
	}})
	sweeper.RunOnce()

	want := []string{"model_abc/config.json", "r0/newest.bin", "r0/pending.bin_9.tmp"}
	got := remaining(t, dir)
	if len(got) != len(want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("files = %v, want %v", got, want)
		}
	}
	assertSample(t, *reported, "gpuworks_kv_disk_used_bytes", "i-1", 300)
}

// One block that cannot be deleted must not stop the sweep: the rest of the
// directory still shrinks, and the reported usage matches what is really left.
func TestKVDiskSweeperContinuesPastARemovalFailure(t *testing.T) {
	dir := t.TempDir()
	stubborn := writeBlock(t, dir, "r0/oldest.bin", 400, 8*time.Hour)
	writeBlock(t, dir, "r0/middle.bin", 400, 5*time.Hour)
	writeBlock(t, dir, "r0/newest.bin", 400, time.Minute)

	// 1200 bytes against a 1000 byte limit: the target is 900.
	sweeper, reported := testSweeper(t, []KVDiskTarget{{
		DeploymentID: 7, Instances: []string{"i-1"}, Engine: "vllm", Dir: dir, LimitBytes: 1000,
	}})
	sweeper.files = refusingFS{refuse: stubborn}
	sweeper.RunOnce()

	// The refused block still occupies its bytes, so the sweeper had to remove
	// the next-oldest one to reach the target.
	want := []string{"r0/newest.bin", "r0/oldest.bin"}
	got := remaining(t, dir)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("files = %v, want %v", got, want)
	}
	assertSample(t, *reported, "gpuworks_kv_disk_used_bytes", "i-1", 800)
}

// Every replica sharing a directory gets the same numbers, because the number
// describes the directory rather than the process.
func TestKVDiskSweeperReportsEveryInstanceSharingTheDirectory(t *testing.T) {
	dir := t.TempDir()
	writeBlock(t, dir, "r0/one.bin", 500, time.Hour)
	sweeper, reported := testSweeper(t, []KVDiskTarget{{
		DeploymentID: 7, Instances: []string{"i-1", "i-2"}, Engine: "vllm", Dir: dir, LimitBytes: 4096,
	}})
	sweeper.RunOnce()

	if len(*reported) != 4 {
		t.Fatalf("samples = %#v, want two per instance", *reported)
	}
	for _, instance := range []string{"i-1", "i-2"} {
		assertSample(t, *reported, "gpuworks_kv_disk_used_bytes", instance, 500)
		assertSample(t, *reported, "gpuworks_kv_disk_limit_bytes", instance, 4096)
	}
	for _, sample := range *reported {
		if sample.Labels["node"] != "node-1" || sample.Labels["engine"] != "vllm" || sample.Labels["deployment"] != "7" {
			t.Fatalf("labels = %#v, want the node, engine and deployment of the target", sample.Labels)
		}
		if sample.TimeMS != 1700000000000 {
			t.Fatalf("timestamp = %d, want the sweep's own clock", sample.TimeMS)
		}
	}
}

// A target with no deployment (DeploymentID 0) is the standalone/legacy case:
// its samples must carry no deployment label at all, not an empty one.
func TestKVDiskSweeperOmitsDeploymentLabelWhenTargetHasNone(t *testing.T) {
	dir := t.TempDir()
	writeBlock(t, dir, "r0/one.bin", 500, time.Hour)
	sweeper, reported := testSweeper(t, []KVDiskTarget{{
		DeploymentID: 0, Instances: []string{"i-1"}, Engine: "vllm", Dir: dir, LimitBytes: 4096,
	}})
	sweeper.RunOnce()
	if len(*reported) == 0 {
		t.Fatal("expected samples to be reported")
	}
	for _, sample := range *reported {
		if _, ok := sample.Labels["deployment"]; ok {
			t.Fatalf("sample %s should carry no deployment label: %#v", sample.Metric, sample.Labels)
		}
	}
}

// A directory with no ceiling is not the sweeper's business: nothing is deleted
// and nothing is claimed about it.
func TestKVDiskSweeperSkipsATargetWithoutALimit(t *testing.T) {
	dir := t.TempDir()
	writeBlock(t, dir, "r0/one.bin", 5000, time.Hour)
	sweeper, reported := testSweeper(t, []KVDiskTarget{{DeploymentID: 7, Instances: []string{"i-1"}, Dir: dir}})
	sweeper.RunOnce()
	if len(*reported) != 0 {
		t.Fatalf("samples = %#v, want none", *reported)
	}
	if got := remaining(t, dir); len(got) != 1 {
		t.Fatalf("files = %v, want the block kept", got)
	}
}

func assertSample(t *testing.T, samples []proto.MetricSample, metric, instance string, want float64) {
	t.Helper()
	for _, sample := range samples {
		if sample.Metric == metric && sample.Labels["instance"] == instance {
			if sample.Value != want {
				t.Fatalf("%s{instance=%s} = %v, want %v", metric, instance, sample.Value, want)
			}
			return
		}
	}
	t.Fatalf("%s{instance=%s} was not reported: %#v", metric, instance, samples)
}

// refusingFS refuses to remove one path and otherwise behaves like the real
// filesystem, which is how a read-only block or a racing engine looks.
type refusingFS struct{ refuse string }

func (f refusingFS) WalkDir(root string, fn fs.WalkDirFunc) error { return filepath.WalkDir(root, fn) }
func (f refusingFS) Remove(name string) error {
	if name == f.refuse {
		return os.ErrPermission
	}
	return os.Remove(name)
}

// --- engine integration -------------------------------------------------

func kvDiskSpec(uuid string, deploymentID uint, limitBytes int64) proto.InstanceDesiredState {
	spec := proto.InstanceDesiredState{UUID: uuid, DeploymentID: deploymentID, Engine: "vllm", DesiredState: "running", APIPort: 30000}
	spec.Mounts = []proto.MountSpec{{Target: "/mnt/kv", Purpose: proto.MountPurposeKVDisk, SizeHint: strconv.FormatInt(limitBytes, 10)}}
	return spec
}

// The server states a container path and a ceiling; the node decides which host
// directory backs it, creates it, and writes it into the workload it runs.
func TestEngineResolvesTheDiskTierMountToAManagedDirectory(t *testing.T) {
	state := t.TempDir()
	t.Setenv(runtimeMountRootEnv, filepath.Join(state, "runtime-mounts"))
	e := NewWithProber(nil, func(proto.InstanceStatus) {}, slog.New(slog.DiscardHandler), state, injectionProber{})

	first := kvDiskSpec("i-1", 42, 4096)
	second := kvDiskSpec("i-2", 42, 4096)
	other := kvDiskSpec("i-3", 43, 4096)

	sources := map[string]string{}
	for _, spec := range []proto.InstanceDesiredState{first, second, other} {
		ws := toWorkloadSpec(spec, "container-"+spec.UUID)
		if err := e.applyKVDiskMount(&ws, spec); err != nil {
			t.Fatal(err)
		}
		mount, found := mountFor(ws, "/mnt/kv")
		if !found || mount.Source == "" {
			t.Fatalf("workload mounts = %#v, want a host source for /mnt/kv", ws.Mounts)
		}
		if mount.ReadOnly {
			t.Fatal("the disk tier mount is read-only; the engine has to write blocks into it")
		}
		info, err := os.Stat(mount.Source)
		if err != nil || !info.IsDir() {
			t.Fatalf("stat %s: %v", mount.Source, err)
		}
		sources[spec.UUID] = mount.Source
	}
	if sources["i-1"] != sources["i-2"] {
		t.Fatalf("replicas of one deployment got %s and %s; they share a cache", sources["i-1"], sources["i-2"])
	}
	if sources["i-1"] == sources["i-3"] {
		t.Fatalf("two deployments share %s; one deployment's cache is invisible to another", sources["i-1"])
	}
}

// An instance without the disk tier gets no managed directory at all.
func TestEngineLeavesOtherMountsAlone(t *testing.T) {
	state := t.TempDir()
	t.Setenv(runtimeMountRootEnv, filepath.Join(state, "runtime-mounts"))
	e := NewWithProber(nil, func(proto.InstanceStatus) {}, slog.New(slog.DiscardHandler), state, injectionProber{})

	spec := proto.InstanceDesiredState{UUID: "i-1", DeploymentID: 42}
	spec.Mounts = []proto.MountSpec{{Source: "/srv/data", Target: "/data", Purpose: "user"}}
	ws := toWorkloadSpec(spec, "container")
	if err := e.applyKVDiskMount(&ws, spec); err != nil {
		t.Fatal(err)
	}
	if mount, _ := mountFor(ws, "/data"); mount.Source != "/srv/data" {
		t.Fatalf("mount = %#v, want the source the server stated", mount)
	}
	if _, err := os.Stat(filepath.Join(state, "runtime-mounts", proto.MountPurposeKVDisk)); !os.IsNotExist(err) {
		t.Fatalf("a kv-disk directory was created for an instance that has no disk tier: %v", err)
	}
}

// The work list is derived from the desired state: one entry per deployment,
// every replica on it, and nothing for a deployment whose ceiling is unknown.
func TestEngineKVDiskTargetsGroupReplicasByDeployment(t *testing.T) {
	state := t.TempDir()
	t.Setenv(runtimeMountRootEnv, filepath.Join(state, "runtime-mounts"))
	e := NewWithProber(nil, func(proto.InstanceStatus) {}, slog.New(slog.DiscardHandler), state, injectionProber{})

	unlimited := kvDiskSpec("i-9", 44, 0)
	unlimited.Mounts[0].SizeHint = ""
	plain := proto.InstanceDesiredState{UUID: "i-4", DeploymentID: 45, DesiredState: "running"}
	e.mu.Lock()
	for _, spec := range []proto.InstanceDesiredState{
		kvDiskSpec("i-2", 42, 4096), kvDiskSpec("i-1", 42, 4096), kvDiskSpec("i-3", 43, 8192), unlimited, plain,
	} {
		e.desired[spec.UUID] = spec
	}
	e.mu.Unlock()

	targets := e.KVDiskTargets()
	if len(targets) != 2 {
		t.Fatalf("targets = %#v, want one per deployment with a ceiling", targets)
	}
	if targets[0].DeploymentID != 42 || len(targets[0].Instances) != 2 ||
		targets[0].Instances[0] != "i-1" || targets[0].Instances[1] != "i-2" {
		t.Fatalf("target = %#v, want both replicas of deployment 42", targets[0])
	}
	if targets[0].LimitBytes != 4096 || targets[0].Dir != e.kvDiskDir(42) {
		t.Fatalf("target = %#v", targets[0])
	}
	if targets[1].DeploymentID != 43 || targets[1].LimitBytes != 8192 {
		t.Fatalf("target = %#v", targets[1])
	}
}

// Removing one replica keeps the shared cache; removing the last one on this
// node releases the disk.
func TestEngineReleasesTheDiskTierDirectoryWithTheLastInstance(t *testing.T) {
	state := t.TempDir()
	t.Setenv(runtimeMountRootEnv, filepath.Join(state, "runtime-mounts"))
	e := NewWithProber(nil, func(proto.InstanceStatus) {}, slog.New(slog.DiscardHandler), state, injectionProber{})

	first, second := kvDiskSpec("i-1", 42, 4096), kvDiskSpec("i-2", 42, 4096)
	ws := toWorkloadSpec(first, "container")
	if err := e.applyKVDiskMount(&ws, first); err != nil {
		t.Fatal(err)
	}
	dir := e.kvDiskDir(42)

	e.mu.Lock()
	e.desired[second.UUID] = second
	e.mu.Unlock()
	e.releaseKVDiskDir(first)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the shared directory went away while a replica still runs: %v", err)
	}

	e.mu.Lock()
	delete(e.desired, second.UUID)
	e.mu.Unlock()
	e.releaseKVDiskDir(second)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("stat %s = %v, want the directory removed", dir, err)
	}
}

func mountFor(ws runtime.WorkloadSpec, target string) (runtime.Mount, bool) {
	for _, mount := range ws.Mounts {
		if mount.Target == target {
			return mount, true
		}
	}
	return runtime.Mount{}, false
}

// An instance deleted while the agent was down never reaches releaseKVDiskDir.
// The sweep after reconnect reclaims the directory of a deployment nothing
// here runs, and nothing else: not a deployment still desired, not a directory
// that is not named like a deployment.
func TestCleanupOrphansReclaimsOnlyTheDiskTierOfADeploymentWithNoDesiredInstance(t *testing.T) {
	state := t.TempDir()
	t.Setenv(runtimeMountRootEnv, filepath.Join(state, "runtime-mounts"))
	e := NewWithProber(&orphanDriver{}, func(proto.InstanceStatus) {}, slog.New(slog.DiscardHandler), state, injectionProber{})

	kept := kvDiskSpec("i-1", 42, 4096)
	ws := toWorkloadSpec(kept, "container")
	if err := e.applyKVDiskMount(&ws, kept); err != nil {
		t.Fatal(err)
	}
	gone := kvDiskSpec("i-2", 43, 4096)
	ws = toWorkloadSpec(gone, "container")
	if err := e.applyKVDiskMount(&ws, gone); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.kvDiskDir(43), "block.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(e.kvDiskDir(42))
	for _, name := range []string{"scratch", "042"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.mu.Lock()
	e.desired[kept.UUID] = kept
	e.mu.Unlock()

	if err := e.CleanupOrphans(context.Background(), e.driver.ListContainers); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.kvDiskDir(43)); !os.IsNotExist(err) {
		t.Fatalf("the directory of a deployment with no instance stayed: %v", err)
	}
	for _, path := range []string{e.kvDiskDir(42), filepath.Join(root, "scratch"), filepath.Join(root, "042")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s was removed though it is not an orphan deployment's directory: %v", path, err)
		}
	}
}

// A container the sweep could not remove may still have its disk tier
// mounted, so the directories stay until a sweep succeeds.
func TestCleanupOrphansLeavesTheDiskTierWhileAnOrphanContainerSurvives(t *testing.T) {
	state := t.TempDir()
	t.Setenv(runtimeMountRootEnv, filepath.Join(state, "runtime-mounts"))
	stubborn := "66666666-7777-8888-9999-000000000000"
	driver := &orphanDriver{
		listed:  []string{containerName(stubborn)},
		stopErr: map[string]error{containerName(stubborn): errors.New("stop timed out")},
	}
	e := NewWithProber(driver, func(proto.InstanceStatus) {}, slog.New(slog.DiscardHandler), state, injectionProber{})
	gone := kvDiskSpec("i-2", 43, 4096)
	ws := toWorkloadSpec(gone, "container")
	if err := e.applyKVDiskMount(&ws, gone); err != nil {
		t.Fatal(err)
	}

	if err := e.CleanupOrphans(context.Background(), e.driver.ListContainers); err == nil {
		t.Fatal("a surviving orphan container was reported as reclaimed")
	}
	if _, err := os.Stat(e.kvDiskDir(43)); err != nil {
		t.Fatalf("the disk tier was removed under a container that may still use it: %v", err)
	}
}
