package gpu

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeKFDTree lays out the KFD sysfs of a two-card K100 host. The CPU node
// and the first card's properties are as read on the Hygon host
// (192.0.2.133, 2026-10-30): gpu_id 50234 at location_id 26368, which is
// 0000:67:00.0.
func writeKFDTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("sys/class/kfd/kfd/topology/nodes/0/gpu_id", "0\n")
	write("sys/class/kfd/kfd/topology/nodes/0/properties", "cpu_cores_count 32\nsimd_count 0\nlocation_id 0\ndomain 0\n")
	write("sys/class/kfd/kfd/topology/nodes/1/gpu_id", "50234\n")
	write("sys/class/kfd/kfd/topology/nodes/1/properties", "simd_count 512\nlocation_id 26368\ndomain 0\n")
	write("sys/class/kfd/kfd/topology/nodes/2/gpu_id", "11111\n")
	write("sys/class/kfd/kfd/topology/nodes/2/properties", "simd_count 512\nlocation_id 34816\ndomain 0\n")
	if err := os.MkdirAll(filepath.Join(root, "sys/class/kfd/kfd/proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestHygonComputeProcessesNoneRunning(t *testing.T) {
	p := &HygonProber{hostRoot: writeKFDTree(t)}
	procs, err := p.ComputeProcesses(t.Context())
	if err != nil || len(procs) != 0 {
		t.Fatalf("ComputeProcesses = %+v, %v", procs, err)
	}
}

func TestHygonComputeProcessesByQueue(t *testing.T) {
	root := writeKFDTree(t)
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// pid 4242 sees both cards (the runtime opens every visible card and
	// holds a little memory on each) but created queues on the second only.
	write("proc/4242/comm", "python3\n")
	write("sys/class/kfd/kfd/proc/4242/vram_50234", "2097152\n")
	write("sys/class/kfd/kfd/proc/4242/vram_11111", "34359738368\n")
	write("sys/class/kfd/kfd/proc/4242/queues/0/gpuid", "11111\n")
	write("sys/class/kfd/kfd/proc/4242/queues/1/gpuid", "11111\n")
	// pid 7 opened /dev/kfd and created no queue anywhere: on no card.
	write("proc/7/comm", "hy-smi\n")
	write("sys/class/kfd/kfd/proc/7/vram_50234", "0\n")
	write("sys/class/kfd/kfd/proc/7/queues/.keep", "")
	// pid 99 exited between the listing and the reads: its comm is gone.
	write("sys/class/kfd/kfd/proc/99/queues/0/gpuid", "50234\n")

	procs, err := (&HygonProber{hostRoot: root}).ComputeProcesses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []ComputeProcess{
		{PID: 4242, Name: "python3", PCIeBDF: "0000:88:00.0", MemoryBytes: 34359738368},
		{PID: 99, Name: "", PCIeBDF: "0000:67:00.0"},
	}
	if !reflect.DeepEqual(procs, want) {
		t.Fatalf("ComputeProcesses = %+v, want %+v", procs, want)
	}
}

func TestHygonComputeProcessesWithoutQueuesFallsBackToMemory(t *testing.T) {
	root := writeKFDTree(t)
	dir := filepath.Join(root, "sys/class/kfd/kfd/proc/31")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{"vram_50234": "1048576", "vram_11111": "0"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	procs, err := (&HygonProber{hostRoot: root}).ComputeProcesses(t.Context())
	want := []ComputeProcess{{PID: 31, PCIeBDF: "0000:67:00.0", MemoryBytes: 1048576}}
	if err != nil || !reflect.DeepEqual(procs, want) {
		t.Fatalf("ComputeProcesses = %+v, %v", procs, err)
	}
}

func TestHygonComputeProcessesUnknownWithoutKFD(t *testing.T) {
	// No KFD at all, or a topology without cards: not knowing must be an
	// error, never an empty listing that reads as every card free.
	if _, err := (&HygonProber{hostRoot: t.TempDir()}).ComputeProcesses(t.Context()); err == nil {
		t.Fatal("a host without KFD listed processes")
	}
	root := writeKFDTree(t)
	if err := os.RemoveAll(filepath.Join(root, "sys/class/kfd/kfd/proc")); err != nil {
		t.Fatal(err)
	}
	if _, err := (&HygonProber{hostRoot: root}).ComputeProcesses(t.Context()); err == nil {
		t.Fatal("a missing KFD process directory read as no processes")
	}
}

func TestKFDNodeBDF(t *testing.T) {
	for props, want := range map[string]string{
		"location_id 26368\ndomain 0\n": "0000:67:00.0",
		"domain 1\nlocation_id 49417\n": "0001:c1:01.1",
		"location_id 0\ndomain 0\n":     "",
		"simd_count 512\n":              "",
		"location_id x\ndomain 0\n":     "",
	} {
		if got := kfdNodeBDF(props); got != want {
			t.Errorf("kfdNodeBDF(%q) = %q, want %q", props, got, want)
		}
	}
}
