//go:build linux

package extplugin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginexec"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// fakeBin is the fake plugin, built once for the package's tests.
var fakeBin string

func TestMain(m *testing.M) {
	RunTrampolineIfAsked()
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "sleep":
			time.Sleep(time.Minute)
			os.Exit(0)
		case "host-helper":
			os.Exit(hostHelper(os.Args[2:]))
		}
	}
	if _, child := os.LookupEnv(envConformancePackage); child {
		// A conformance child runs the package its parent laid out.
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "extplugin-pkg-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if fakeBin, err = BuildFakePlugin(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// recordingState acknowledges every push and remembers who made it.
type recordingState struct {
	mu     sync.Mutex
	owners []string
	kinds  []string
}

func (s *recordingState) Held(string) []extapi.HeldState { return nil }

func (s *recordingState) Push(_ context.Context, owner pluginhost.Owner, _ string, p *extapi.StatePush) (*extapi.StateAck, error) {
	if _, err := p.DecodeBody(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.owners, s.kinds = append(s.owners, owner.ID), append(s.kinds, p.Kind)
	s.mu.Unlock()
	c, err := extapi.Canonical(p.Body)
	if err != nil {
		return nil, err
	}
	at := extapi.NewTime(time.Now())
	return &extapi.StateAck{Kind: p.Kind, Generation: p.Generation, Digest: extapi.DigestOf(c), AppliedAt: &at}, nil
}

func core() extapi.CoreInfo {
	return extapi.CoreInfo{Version: "v26.12.0", InstallID: "inst_extplugin", OS: "linux", Arch: runtime.GOARCH, HARole: extapi.HARoleSingle,
		Offer: map[string]extapi.Range{"table.banners": {Min: 1, Max: 1}, "table.node_holds": {Min: 1, Max: 1}}}
}

func timing() pluginhost.Timing {
	return pluginhost.Timing{InitTimeout: 5 * time.Second, HealthEvery: 100 * time.Millisecond, HealthTimeout: 500 * time.Millisecond,
		BackoffMin: 20 * time.Millisecond, BackoffMax: 80 * time.Millisecond, CrashloopCount: 4, CrashloopWindow: time.Minute,
		CrashloopRetry: 500 * time.Millisecond, StopGrace: 2 * time.Second, TermGrace: time.Second}
}

func newHost(t *testing.T, state pluginhost.StateHost, tm pluginhost.Timing) *pluginhost.Host {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h, err := pluginhost.New(pluginhost.Config{Trampoline: self, Core: core, State: state,
		Log: slog.New(slog.DiscardHandler), Timing: tm})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := h.Close(ctx); err != nil {
			t.Errorf("closing the host: %v", err)
		}
	})
	return h
}

func fakeSpec(t *testing.T, id string, args ...string) pluginhost.Spec {
	return pluginhost.Spec{ID: id, Version: "1.0.0", ManifestSHA256: FakeManifest, Exe: fakeBin,
		Args:       append([]string{"serve", "--id", id, "--manifest", FakeManifest}, args...),
		PackageDir: filepath.Dir(fakeBin), DataDir: t.TempDir(), TmpDir: t.TempDir(), Lifecycle: extapi.LifecycleResident,
		Grant:           extapi.PluginGrant{ID: id, Version: "1.0.0"},
		Approved:        map[string]int{"table.banners": 1, "table.node_holds": 1},
		RequireLandlock: pluginexec.LandlockABI() > 0}
}

func waitStatus(t *testing.T, h *pluginhost.Host, id string, what string, cond func(pluginhost.Status) bool) pluginhost.Status {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, _ := h.Status(id)
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: never %s; last status %+v; stderr:\n%s", id, what, st, h.Stderr(id))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func running(st pluginhost.Status) bool { return st.State == pluginhost.StateRunning }

// probeReport waits for the plugin's probe line on stderr.
func probeReport(t *testing.T, h *pluginhost.Host, id, probe string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		sc := bufio.NewScanner(bytes.NewReader(h.Stderr(id)))
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var rec map[string]any
			if json.Unmarshal(sc.Bytes(), &rec) == nil && rec["msg"] == "probe" && rec["probe"] == probe {
				return rec
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s: no %s probe on stderr:\n%s", id, probe, h.Stderr(id))
	return nil
}

func requireLandlock(t *testing.T) {
	t.Helper()
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	t.Logf("Landlock ABI probe: result=%d errno=%d (%s)", abi, errno, errno.Error())
	if status, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "NoNewPrivs:") || strings.HasPrefix(line, "Seccomp:") || strings.HasPrefix(line, "Seccomp_filters:") {
				t.Log(line)
			}
		}
	} else {
		t.Logf("read sandbox process status: %v", err)
	}
	if pluginexec.LandlockABI() == 0 {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal("Landlock is unavailable to this process; GPUWORKS_TEST_STRICT=1 requires it (see probe diagnostics)")
		}
		t.Skip("Landlock is unavailable to this process (see probe diagnostics)")
	}
}

// A plugin started through the trampoline pushes state that is applied
// for the package its connection belongs to.
func TestProcessPushesAsItsPackage(t *testing.T) {
	state := &recordingState{}
	h := newHost(t, state, timing())
	body := `{"banners":[{"audience":"admin","id":"hello","steps":[{"notice":{"code":"ACME_NOTICE"},"severity":"info"}]}]}`
	if err := h.Start(fakeSpec(t, "acme_fake", "--push", "banners="+body)); err != nil {
		t.Fatal(err)
	}
	st := waitStatus(t, h, "acme_fake", "running", running)
	if pluginexec.LandlockABI() > 0 && st.Isolation != "landlock" {
		t.Errorf("isolation = %q", st.Isolation)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		state.mu.Lock()
		n := len(state.owners)
		state.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.owners) != 1 || state.owners[0] != "acme_fake" || state.kinds[0] != "banners" {
		t.Fatalf("pushes by %v of %v; stderr:\n%s", state.owners, state.kinds, h.Stderr("acme_fake"))
	}
}

// A plugin that refuses initialize is not restarted.
func TestProcessHandshakeRefusal(t *testing.T) {
	h := newHost(t, nil, timing())
	_ = h.Start(fakeSpec(t, "acme_fake", "--refuse", "ACME_NOT_CONFIGURED:failed_precondition"))
	st := waitStatus(t, h, "acme_fake", "failed", func(s pluginhost.Status) bool { return s.State == pluginhost.StateFailed })
	if st.Code != messages.ErrorPluginHandshakeRefused || st.Params["plugin_code"] != "ACME_NOT_CONFIGURED" || st.Starts != 1 {
		t.Fatalf("status = %+v", st)
	}
	h2 := newHost(t, nil, timing())
	spec := fakeSpec(t, "acme_fake", "--requires", "host.fleet@1-1")
	_ = h2.Start(spec)
	st = waitStatus(t, h2, "acme_fake", "failed", func(s pluginhost.Status) bool { return s.State == pluginhost.StateFailed })
	if st.Params["plugin_code"] != extapi.ErrorCapabilityMissing {
		t.Fatalf("status = %+v", st)
	}
}

// A plugin that exits at once is restarted after growing delays until it
// crashloops, where retries continue; its last words reach the stderr ring.
func TestProcessCrashloop(t *testing.T) {
	h := newHost(t, nil, timing())
	_ = h.Start(fakeSpec(t, "acme_fake", "--exit-at-start", "3"))
	st := waitStatus(t, h, "acme_fake", "in crashloop", func(s pluginhost.Status) bool { return s.State == pluginhost.StateCrashloop })
	if st.Code != messages.ErrorPluginCrashloop || st.Params["crashes"] != 4 {
		t.Fatalf("status = %+v", st)
	}
	if !strings.Contains(string(h.Stderr("acme_fake")), "exiting at start on purpose") {
		t.Fatalf("stderr ring:\n%s", h.Stderr("acme_fake"))
	}
	starts := st.Starts
	waitStatus(t, h, "acme_fake", "retried", func(s pluginhost.Status) bool { return s.Starts > starts })
}

// A plugin that never answers initialize, or stops answering health, is
// killed and restarted.
func TestProcessHangs(t *testing.T) {
	t.Run("init", func(t *testing.T) {
		tm := timing()
		tm.InitTimeout = 300 * time.Millisecond
		h := newHost(t, nil, tm)
		_ = h.Start(fakeSpec(t, "acme_fake", "--hang", "init"))
		st := waitStatus(t, h, "acme_fake", "backing off", func(s pluginhost.Status) bool { return s.State == pluginhost.StateBackoff })
		if st.Code != messages.ErrorPluginHandshakeTimeout {
			t.Fatalf("status = %+v", st)
		}
	})
	t.Run("health", func(t *testing.T) {
		h := newHost(t, nil, timing())
		_ = h.Start(fakeSpec(t, "acme_fake", "--hang", "health"))
		waitStatus(t, h, "acme_fake", "running", running)
		waitStatus(t, h, "acme_fake", "restarted", func(s pluginhost.Status) bool { return s.Starts >= 2 })
	})
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// startOnThreadThatExits starts cmd from a goroutine that locks its thread
// and ends without unlocking, and returns that thread's id. The runtime
// ends such a thread with its goroutine, with one exception: the process's
// main thread is parked for good instead, since ending it would leave the
// process an unwaitable zombie. A goroutine that finds itself there holds
// the main thread until another goroutine, which then has to run elsewhere,
// has started the child.
func startOnThreadThatExits(cmd *exec.Cmd) (tid int, err error) {
	type result struct {
		tid int
		err error
	}
	done := make(chan result)
	go func() {
		runtime.LockOSThread()
		if unix.Gettid() == unix.Getpid() {
			defer runtime.UnlockOSThread()
			tid, err := startOnThreadThatExits(cmd)
			done <- result{tid, err}
			return
		}
		// Never unlocked: the thread exits with the goroutine.
		done <- result{unix.Gettid(), cmd.Start()}
	}()
	r := <-done
	return r.tid, r.err
}

// The parent-death signal follows the thread that forked the child: when
// a goroutine that locked its thread spawns a child and ends, the runtime
// ends the thread and the kernel kills the child. This is the hazard
// the plugin host's spawning thread must avoid.
func TestParentDeathSignalFollowsTheForkingThread(t *testing.T) {
	self, _ := os.Executable()
	cmd := exec.Command(self, "sleep")
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	tid, err := startOnThreadThatExits(cmd)
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err = <-waited:
	case <-time.After(10 * time.Second):
		_, statErr := os.Stat(fmt.Sprintf("/proc/self/task/%d", tid))
		_ = cmd.Process.Kill()
		<-waited
		t.Fatalf("child %d is alive 10s after the goroutine that forked it ended: thread %d should have exited with that goroutine and taken the child with it (thread still exists: %v, main thread is %d)",
			cmd.Process.Pid, tid, statErr == nil, unix.Getpid())
	}
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("the child forked by exited thread %d ended with %v (%v), want SIGKILL from the kernel", tid, cmd.ProcessState, err)
	}
}

// The host's spawning goroutine keeps its thread for the plugin's life, so
// retiring every other locked thread leaves the plugin running.
func TestPluginSurvivesThreadRetirement(t *testing.T) {
	h := newHost(t, nil, timing())
	_ = h.Start(fakeSpec(t, "acme_fake"))
	st := waitStatus(t, h, "acme_fake", "running", running)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.LockOSThread() // retired when this goroutine returns
			time.Sleep(time.Millisecond)
		}()
	}
	wg.Wait()
	time.Sleep(500 * time.Millisecond) // several health periods
	after, _ := h.Status("acme_fake")
	if after.State != pluginhost.StateRunning || after.PID != st.PID || after.Starts != 1 || !alive(st.PID) {
		t.Fatalf("after retiring threads: %+v (before %+v)", after, st)
	}
}

// hostHelper is a minimal server: it runs the fake plugin through the
// trampoline, prints the plugin's pid and waits to be killed.
func hostHelper(args []string) int {
	self, _ := os.Executable()
	h, err := pluginhost.New(pluginhost.Config{Trampoline: self, Core: core, Log: slog.New(slog.DiscardHandler), Timing: timing()})
	if err != nil {
		return 1
	}
	spec := pluginhost.Spec{ID: "acme_fake", Version: "1.0.0", ManifestSHA256: FakeManifest, Exe: args[0],
		Args:       []string{"serve", "--manifest", FakeManifest},
		PackageDir: filepath.Dir(args[0]), DataDir: args[1], TmpDir: args[1], Lifecycle: extapi.LifecycleResident,
		Grant: extapi.PluginGrant{ID: "acme_fake", Version: "1.0.0"}, Approved: map[string]int{"table.banners": 1}}
	if err := h.Start(spec); err != nil {
		return 1
	}
	for {
		if st, _ := h.Status("acme_fake"); st.State == pluginhost.StateRunning {
			fmt.Println(st.PID)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {}
}

// A server killed outright takes its plugins with it.
func TestServerDeathKillsThePlugin(t *testing.T) {
	self, _ := os.Executable()
	cmd := exec.Command(self, "host-helper", fakeBin, t.TempDir())
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("the helper printed no plugin pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || !alive(pid) {
		_ = cmd.Process.Kill()
		t.Fatalf("plugin pid %q", line)
	}
	_ = cmd.Process.Signal(syscall.SIGKILL)
	_ = cmd.Wait()
	deadline := time.Now().Add(10 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("plugin %d outlived its server", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Under Landlock a plugin cannot read a file outside its package, data and
// tmp directories (the server's master.key), while it can write its data.
func TestLandlockDeniesPathsOutsideThePlugin(t *testing.T) {
	requireLandlock(t)
	secret := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(secret, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHost(t, nil, timing())
	spec := fakeSpec(t, "acme_fake", "--probe-open", secret)
	_ = h.Start(spec)
	rep := probeReport(t, h, "acme_fake", "files")
	if rep["read"] != "EACCES" {
		t.Fatalf("reading %s from the plugin: %v, want EACCES", secret, rep["read"])
	}
	if rep["write"] != "" {
		t.Fatalf("writing the data directory: %v", rep["write"])
	}
	if b, err := os.ReadFile(filepath.Join(spec.DataDir, "probe.txt")); err != nil || string(b) != "ok" {
		t.Fatalf("the plugin's data write did not land: %v", err)
	}
}

// socketInodes lists the socket inodes a process holds, read from /proc.
func socketInodes(t *testing.T, pid int) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, e := range entries {
		link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, e.Name()))
		if err == nil && strings.HasPrefix(link, "socket:[") {
			out[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] = true
		}
	}
	return out
}

// A plugin holds only its own two sockets on every host. With Landlock it
// also cannot open a sibling's descriptors through /proc or (ABI 6+) signal it.
func TestSiblingCannotReachAnotherPluginsSockets(t *testing.T) {
	h := newHost(t, nil, timing())
	_ = h.Start(fakeSpec(t, "acme_one"))
	one := waitStatus(t, h, "acme_one", "running", running)
	oneSockets := socketInodes(t, one.PID)
	if len(oneSockets) != 2 {
		t.Fatalf("plugin one holds sockets %v, want its two", oneSockets)
	}
	_ = h.Start(fakeSpec(t, "acme_two", "--probe-pid", strconv.Itoa(one.PID)))
	rep := probeReport(t, h, "acme_two", "sibling")
	theirs := strings.Split(rep["sockets"].(string), ",")
	if len(theirs) != 2 {
		t.Fatalf("plugin two holds sockets %v, want only its own two", theirs)
	}
	for _, ino := range theirs {
		if oneSockets[ino] {
			t.Fatalf("plugin two holds plugin one's socket %s", ino)
		}
	}
	t.Run("Landlock blocks sibling access", func(t *testing.T) {
		requireLandlock(t)
		if rep["opened"] != "" {
			t.Fatalf("plugin two opened plugin one's descriptors %v", rep["opened"])
		}
		if pluginexec.LandlockABI() >= 6 && rep["signal"] != "EPERM" {
			t.Fatalf("plugin two signalling plugin one: %v, want EPERM", rep["signal"])
		}
	})
	if !alive(one.PID) {
		t.Fatal("plugin one died")
	}
}
