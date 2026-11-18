package pluginhost

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
)

// fakeMemory answers GroupRSS with rss, or err, and counts the reads.
type fakeMemory struct {
	mu    sync.Mutex
	rss   int64
	err   error
	reads []int
}

func (m *fakeMemory) GroupRSS(pgid int) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads = append(m.reads, pgid)
	return m.rss, m.err
}

func (m *fakeMemory) set(rss int64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rss, m.err = rss, err
}

func (m *fakeMemory) groups() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.reads)
}

// syncBuffer is a log destination the host's goroutines write to while the
// test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func memoryTiming() Timing {
	t := fastTiming()
	t.MemoryEvery = 5 * time.Millisecond
	return t
}

// newMemoryHost is newTestHost with cfg's memory reader and cgroup, and a
// log the test reads.
func newMemoryHost(t *testing.T, sp *fakeSpawner, cfg Config) (*Host, *statusLog, *syncBuffer) {
	t.Helper()
	log, out := &statusLog{}, &syncBuffer{}
	cfg.Spawner, cfg.Core, cfg.OnState, cfg.Timing = sp, testCore, log.add, memoryTiming()
	cfg.Log = slog.New(slog.NewTextHandler(out, nil))
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := h.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return h, log, out
}

func plainSpawner() *fakeSpawner {
	return &fakeSpawner{config: func(int) pluginsdk.Config { return sdkConfig("acme_backup") }}
}

func TestRSSWatchdogRestartsAPluginAboveOneAndAHalfTimesItsLimit(t *testing.T) {
	mem := &fakeMemory{rss: 193 << 20} // the limit is 128 MiB, the watchdog's bound 192 MiB
	sp := plainSpawner()
	h, log, _ := newMemoryHost(t, sp, Config{Memory: mem})
	if err := h.Start(testSpec(t, "acme_backup")); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, h, "acme_backup", func(s Status) bool { return s.Starts >= 2 })
	if sent := sp.proc(1).sent(); !slices.Contains(sent, os.Signal(syscall.SIGKILL)) {
		t.Fatalf("the first process got %v, want SIGKILL to its group", sent)
	}
	if g := mem.groups(); len(g) == 0 || g[0] != sp.proc(1).PID() {
		t.Fatalf("the watchdog read groups %v, want the plugin's own (%d)", g, sp.proc(1).PID())
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	for _, s := range log.all {
		if s.State == StateBackoff && s.Code == messages.ErrorPluginMemoryExceeded {
			if s.Params["id"] != "acme_backup" || s.Params["rss_mib"] != int64(193) || s.Params["limit_mib"] != 128 {
				t.Fatalf("params = %#v", s.Params)
			}
			if s.RSSBytes != 0 || s.Cgroup != "" {
				t.Fatalf("a plugin that no longer runs keeps its last run's memory: rss %d, cgroup %q", s.RSSBytes, s.Cgroup)
			}
			return
		}
	}
	t.Fatal("no backoff named PLUGIN_MEMORY_EXCEEDED")
}

func TestRSSWatchdogLeavesAPluginWithinItsBoundRunning(t *testing.T) {
	mem := &fakeMemory{rss: 192 << 20} // exactly 1.5 × 128 MiB is still allowed
	sp := plainSpawner()
	h, _, _ := newMemoryHost(t, sp, Config{Memory: mem})
	_ = h.Start(testSpec(t, "acme_backup"))
	st := waitStatus(t, h, "acme_backup", func(s Status) bool { return s.State == StateRunning && s.RSSBytes == 192<<20 })
	if st.Cgroup != "" {
		t.Fatalf("Cgroup = %q without a cgroup", st.Cgroup)
	}
	for len(mem.groups()) < 10 {
		time.Sleep(time.Millisecond)
	}
	if st, _ := h.Status("acme_backup"); st.Starts != 1 || st.State != StateRunning {
		t.Fatalf("a plugin within its bound was restarted: %+v", st)
	}
	// Growing past it later still counts.
	mem.set(300<<20, nil)
	waitStatus(t, h, "acme_backup", func(s Status) bool { return s.Starts == 2 })
}

func TestRSSWatchdogLogsAReadFailureOnceAndKeepsThePlugin(t *testing.T) {
	mem := &fakeMemory{err: errors.New("proc unreadable")}
	sp := plainSpawner()
	h, _, out := newMemoryHost(t, sp, Config{Memory: mem})
	_ = h.Start(testSpec(t, "acme_backup"))
	waitStatus(t, h, "acme_backup", inState(StateRunning))
	for len(mem.groups()) < 10 {
		time.Sleep(time.Millisecond)
	}
	if st, _ := h.Status("acme_backup"); st.Starts != 1 || st.State != StateRunning || st.RSSBytes != 0 {
		t.Fatalf("an unreadable plugin was disturbed: %+v", st)
	}
	if n := strings.Count(out.String(), "plugin memory could not be read"); n != 1 {
		t.Fatalf("the read failure was logged %d times, want once:\n%s", n, out.String())
	}
	// A read that works again counts, and a new failure is logged again.
	mem.set(10<<20, nil)
	waitStatus(t, h, "acme_backup", func(s Status) bool { return s.RSSBytes == 10<<20 })
	mem.set(0, errors.New("proc unreadable"))
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(out.String(), "plugin memory could not be read") != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("a second failure after a good read was not logged:\n%s", out.String())
		}
		time.Sleep(time.Millisecond)
	}
}

// fakePluginCgroup lays out <root>/svc/plugins/<id> holding pid, with a
// memory limit, as cgroupfs shows a plugin the trampoline moved.
func fakePluginCgroup(t *testing.T, id string, pid int, memoryMax string) (root, dir string) {
	t.Helper()
	root = t.TempDir()
	dir = filepath.Join(root, "svc", "plugins", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte("1\n"+strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if memoryMax != "" {
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(memoryMax+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, dir
}

func TestNoRSSWatchdogForAPluginInItsOwnCgroup(t *testing.T) {
	mem := &fakeMemory{rss: 1 << 40}
	sp := plainSpawner()
	root, dir := fakePluginCgroup(t, "acme_backup", 1001, "134217728") // the fake spawner's first pid
	h, _, _ := newMemoryHost(t, sp, Config{Memory: mem, CgroupRoot: root, CgroupParent: "svc"})
	_ = h.Start(testSpec(t, "acme_backup"))
	st := waitStatus(t, h, "acme_backup", inState(StateRunning))
	if st.Cgroup != "svc/plugins/acme_backup" {
		t.Fatalf("Cgroup = %q", st.Cgroup)
	}
	sp.mu.Lock()
	p := sp.reqs[0].Profile.Cgroup
	sp.mu.Unlock()
	if p == nil || p.MemoryMax != 128<<20 || p.Parent != "svc" {
		t.Fatalf("the trampoline was not asked for the cgroup: %+v", p)
	}
	time.Sleep(50 * time.Millisecond) // ten watchdog periods
	if g := mem.groups(); len(g) != 0 {
		t.Fatalf("the watchdog read %v for a plugin its cgroup bounds", g)
	}
	if st, _ := h.Status("acme_backup"); st.Starts != 1 || st.RSSBytes != 0 {
		t.Fatalf("status = %+v", st)
	}
	// cgroupfs empties a cgroup whose processes are gone; stopping removes
	// the directory.
	for _, f := range []string{"cgroup.procs", "memory.max"} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Stop(context.Background(), "acme_backup"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the plugin's cgroup outlived its stop: %v", err)
	}
}

func TestRSSWatchdogRunsWhenTheCgroupMoveDidNotHappen(t *testing.T) {
	for name, tc := range map[string]struct {
		pid       int
		memoryMax string
	}{
		"not moved":            {pid: 4242, memoryMax: "134217728"},
		"no memory controller": {pid: 1001},
		"limit not written":    {pid: 1001, memoryMax: "max"},
	} {
		t.Run(name, func(t *testing.T) {
			mem := &fakeMemory{rss: 1 << 20}
			root, _ := fakePluginCgroup(t, "acme_backup", tc.pid, tc.memoryMax)
			h, _, _ := newMemoryHost(t, plainSpawner(), Config{Memory: mem, CgroupRoot: root, CgroupParent: "svc"})
			_ = h.Start(testSpec(t, "acme_backup"))
			st := waitStatus(t, h, "acme_backup", func(s Status) bool { return s.State == StateRunning && s.RSSBytes == 1<<20 })
			if st.Cgroup != "" {
				t.Fatalf("Cgroup = %q for a plugin its cgroup does not bound", st.Cgroup)
			}
		})
	}
}

func TestStoppingAnUnsupervisedPluginRemovesItsLeftoverCgroup(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "svc", "plugins", "acme_backup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	h, _, _ := newMemoryHost(t, plainSpawner(), Config{Memory: &fakeMemory{}, CgroupRoot: root, CgroupParent: "svc"})
	if err := h.Stop(context.Background(), "acme_backup"); !errors.Is(err, ErrUnknownPlugin) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("a leftover cgroup survived: %v", err)
	}
	// An id that is no package id never reaches the filesystem.
	if err := h.Stop(context.Background(), "../svc"); !errors.Is(err, ErrUnknownPlugin) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "svc")); err != nil {
		t.Fatalf("stopping a non-id touched the service cgroup: %v", err)
	}
}

func TestParseProcStatAndPs(t *testing.T) {
	// A command name with spaces and parentheses; pgrp 4242, rss 2560 pages.
	stat := "4243 (acme (worker) x) S 4242 4242 4242 0 -1 4194560 100 0 0 0 1 2 0 0 25 5 3 0 123 52428800 2560 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0\n"
	g, rss, ok := parseProcStat([]byte(stat))
	if !ok || g != 4242 || rss != 2560 {
		t.Fatalf("parseProcStat = %d, %d, %v", g, rss, ok)
	}
	if _, _, ok := parseProcStat([]byte("4243 (short) S 1 2")); ok {
		t.Fatal("a truncated stat parsed")
	}
	ps := []byte("    1  1200\n 4242 20480\n 4242 10240\n  77 999999\nbad line here\n")
	if got, err := parsePsRSS(ps, 4242); err != nil || got != 30720<<10 {
		t.Fatalf("parsePsRSS = %d, %v", got, err)
	}
	if _, err := parsePsRSS(ps, 5); err == nil {
		t.Fatal("a group with no process has no RSS")
	}
}
