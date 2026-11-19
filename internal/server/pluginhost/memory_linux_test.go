//go:build linux

package pluginhost

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// fakeProc lays out a /proc with one stat file per process: pid, its
// process group and its resident MiB.
func fakeProc(t *testing.T, procs ...[3]int) string {
	t.Helper()
	root := t.TempDir()
	page := os.Getpagesize()
	for _, p := range procs {
		dir := filepath.Join(root, fmt.Sprint(p[0]))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		stat := fmt.Sprintf("%d (fake plugin) S 1 %d %d 0 -1 0 0 0 0 0 0 0 0 0 25 5 1 0 1 1 %d 0\n", p[0], p[1], p[1], p[2]<<20/page)
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Entries that are not processes are skipped.
	if err := os.WriteFile(filepath.Join(root, "meminfo"), []byte("MemTotal: 1 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestProcMemorySumsTheWholeProcessGroup(t *testing.T) {
	proc := fakeProc(t, [3]int{1001, 1001, 100}, [3]int{1002, 1001, 40}, [3]int{1003, 1001, 60}, [3]int{2000, 2000, 900})
	got, err := procMemory{proc: proc}.GroupRSS(1001)
	if err != nil || got != 200<<20 {
		t.Fatalf("GroupRSS = %d MiB, %v; want 200 MiB, the leader and both children", got>>20, err)
	}
	if _, err := (procMemory{proc: proc}).GroupRSS(3000); err == nil {
		t.Fatal("a group without processes has no RSS")
	}
	if _, err := (procMemory{proc: filepath.Join(proc, "missing")}).GroupRSS(1001); err == nil {
		t.Fatal("an unreadable /proc has no RSS")
	}
}

// The plugin process itself stays under the bound; only its child pushes
// the group over it, and the group is what the watchdog kills.
func TestRSSWatchdogCountsThePluginsChildren(t *testing.T) {
	proc := fakeProc(t, [3]int{1001, 1001, 120}, [3]int{1005, 1001, 80}, [3]int{1500, 1500, 4000})
	sp := plainSpawner()
	h, log, _ := newMemoryHost(t, sp, Config{Memory: procMemory{proc: proc}})
	_ = h.Start(testSpec(t, "acme_backup")) // mem_mib 128: bound 192 MiB, the plugin alone holds 120
	waitStatus(t, h, "acme_backup", func(s Status) bool { return s.Starts >= 2 })
	log.mu.Lock()
	defer log.mu.Unlock()
	for _, s := range log.all {
		if s.Code == messages.ErrorPluginMemoryExceeded {
			if s.Params["rss_mib"] != int64(200) {
				t.Fatalf("rss_mib = %v, want the group's 200", s.Params["rss_mib"])
			}
			return
		}
	}
	t.Fatal("the group over its bound was not killed")
}
