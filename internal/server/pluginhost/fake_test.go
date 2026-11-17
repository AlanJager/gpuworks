package pluginhost

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
)

const testDigest = extapi.Digest("sha256:69a8fc5cf9fa0174e3dfe74b1c9c1f5f4f64710afa362419883f19c6b42ee422")

// fakeSpawner runs each "process" as pluginsdk.Serve in a goroutine over
// real socketpairs, so the host speaks real gwx/1 to a real SDK. Process n
// (1-based) runs config(n); spawnErr(n), when set, fails that spawn.
type fakeSpawner struct {
	config   func(n int) pluginsdk.Config
	spawnErr func(n int) error
	// linger keeps a process alive after its Serve returned until it is
	// signalled; ignoreTerm makes it survive SIGTERM too.
	linger, ignoreTerm bool

	mu      sync.Mutex
	n       int
	procs   []*fakeProcess
	spawned []time.Time
	reqs    []SpawnRequest
}

func (f *fakeSpawner) Spawn(_ context.Context, req SpawnRequest) (Process, error) {
	f.mu.Lock()
	f.n++
	n := f.n
	f.spawned = append(f.spawned, time.Now())
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	if f.spawnErr != nil {
		if err := f.spawnErr(n); err != nil {
			return nil, err
		}
	}
	serveCore, servePlugin, err := connPair()
	if err != nil {
		return nil, err
	}
	hostCore, hostPlugin, err := connPair()
	if err != nil {
		return nil, err
	}
	r, w := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	p := &fakeProcess{pid: 1000 + n, serve: serveCore, host: hostCore, stderrR: r, stderrW: w,
		pluginConns: []net.Conn{servePlugin, hostPlugin}, done: make(chan struct{}), cancel: cancel,
		linger: f.linger, ignoreTerm: f.ignoreTerm}
	f.mu.Lock()
	f.procs = append(f.procs, p)
	f.mu.Unlock()
	cfg := f.config(n)
	cfg.Logger = slog.New(slog.NewJSONHandler(w, nil))
	go func() {
		err := pluginsdk.Serve(ctx, cfg, servePlugin, hostPlugin)
		code := 0
		if err != nil {
			code = 1
		}
		if !p.linger {
			p.finish(ExitStatus{Code: code})
		}
	}()
	return p, nil
}

func (f *fakeSpawner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

func (f *fakeSpawner) proc(n int) *fakeProcess {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.procs[n-1]
}

func connPair() (net.Conn, net.Conn, error) {
	a, b, err := socketPair()
	if err != nil {
		return nil, nil, err
	}
	ca, err := fileConn(a)
	if err != nil {
		return nil, nil, err
	}
	cb, err := fileConn(b)
	if err != nil {
		return nil, nil, err
	}
	return ca, cb, nil
}

type fakeProcess struct {
	pid         int
	serve, host net.Conn
	pluginConns []net.Conn
	stderrR     *io.PipeReader
	stderrW     *io.PipeWriter
	done        chan struct{}
	cancel      context.CancelFunc
	linger      bool
	ignoreTerm  bool
	once        sync.Once
	mu          sync.Mutex
	exit        ExitStatus
	signals     []os.Signal
}

func (p *fakeProcess) PID() int              { return p.pid }
func (p *fakeProcess) Serve() net.Conn       { return p.serve }
func (p *fakeProcess) Host() net.Conn        { return p.host }
func (p *fakeProcess) Stderr() io.Reader     { return p.stderrR }
func (p *fakeProcess) Done() <-chan struct{} { return p.done }

func (p *fakeProcess) Exit() ExitStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exit
}

func (p *fakeProcess) Signal(sig os.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, sig)
	p.mu.Unlock()
	switch {
	case sig == syscall.SIGKILL:
		p.finish(ExitStatus{Code: -1, Signal: "killed"})
	case sig == syscall.SIGTERM && !p.ignoreTerm:
		p.finish(ExitStatus{Code: -1, Signal: "terminated"})
	}
	return nil
}

func (p *fakeProcess) sent() []os.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]os.Signal(nil), p.signals...)
}

// finish ends the process: its sockets close as the kernel would close
// them, its stderr reaches EOF.
func (p *fakeProcess) finish(st ExitStatus) {
	p.once.Do(func() {
		p.cancel()
		for _, c := range p.pluginConns {
			_ = c.Close()
		}
		p.mu.Lock()
		p.exit = st
		p.mu.Unlock()
		_ = p.stderrW.Close()
		close(p.done)
	})
}

// fakeState records pushes and acknowledges them, or refuses with refuse.
type fakeState struct {
	mu     sync.Mutex
	held   []extapi.HeldState
	pushes []recordedPush
	refuse error
	ack    func(p *extapi.StatePush) *extapi.StateAck
}

type recordedPush struct {
	owner  Owner
	method string
	push   extapi.StatePush
}

func (s *fakeState) Held(string) []extapi.HeldState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held
}

func (s *fakeState) Push(_ context.Context, owner Owner, method string, push *extapi.StatePush) (*extapi.StateAck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pushes = append(s.pushes, recordedPush{owner: owner, method: method, push: *push})
	if s.refuse != nil {
		return nil, s.refuse
	}
	if s.ack != nil {
		return s.ack(push), nil
	}
	at := extapi.NewTime(time.Now())
	c, err := extapi.Canonical(push.Body)
	if err != nil {
		return nil, err
	}
	return &extapi.StateAck{Kind: push.Kind, Generation: push.Generation, Digest: extapi.DigestOf(c), AppliedAt: &at}, nil
}

func (s *fakeState) recorded() []recordedPush {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedPush(nil), s.pushes...)
}

func testCore() extapi.CoreInfo {
	return extapi.CoreInfo{Version: "v26.12.0", InstallID: "inst_test_0001", OS: "linux", Arch: "amd64", HARole: extapi.HARoleSingle,
		Offer: map[string]extapi.Range{"table.banners": {Min: 1, Max: 1}, "table.node_holds": {Min: 1, Max: 1}, "routes": {Min: 1, Max: 1}}}
}

func testSpec(t *testing.T, id string) Spec {
	dir := t.TempDir()
	return Spec{ID: id, Version: "1.0.0", ManifestSHA256: testDigest, Exe: "/pkg/bin/" + id,
		PackageDir: "/pkg", DataDir: dir, TmpDir: dir, Lifecycle: extapi.LifecycleResident,
		Grant:    extapi.PluginGrant{ID: id, Version: "1.0.0", PermissionsGranted: []string{"push:banners"}},
		Approved: map[string]int{"table.banners": 1, "table.node_holds": 1, "routes": 1}}
}

func sdkConfig(id string) pluginsdk.Config {
	return pluginsdk.Config{ID: id, Version: "1.0.0", ManifestSHA256: testDigest,
		Requires: map[string]extapi.Range{"table.banners": {Min: 1, Max: 1}}}
}

func fastTiming() Timing {
	return Timing{InitTimeout: 2 * time.Second, HealthEvery: 20 * time.Millisecond, HealthTimeout: 50 * time.Millisecond,
		BackoffMin: 10 * time.Millisecond, BackoffMax: 40 * time.Millisecond, HealthyReset: time.Hour,
		CrashloopCount: 50, CrashloopWindow: time.Minute, CrashloopRetry: time.Second,
		StopGrace: 300 * time.Millisecond, TermGrace: 100 * time.Millisecond, IdleStop: time.Hour, CallTimeout: 5 * time.Second}
}

// statusLog records every status OnState reports.
type statusLog struct {
	mu  sync.Mutex
	all []Status
}

func (l *statusLog) add(s Status) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all = append(l.all, s)
}

func (l *statusLog) states(id string) []State {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []State
	for _, s := range l.all {
		if s.ID == id && (len(out) == 0 || out[len(out)-1] != s.State) {
			out = append(out, s.State)
		}
	}
	return out
}

func newTestHost(t *testing.T, sp *fakeSpawner, state StateHost, timing Timing) (*Host, *statusLog) {
	t.Helper()
	log := &statusLog{}
	var st StateHost
	if state != nil {
		st = state
	}
	h, err := New(Config{Spawner: sp, Core: testCore, State: st, Log: slog.New(slog.DiscardHandler), OnState: log.add, Timing: timing})
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
	return h, log
}

// waitStatus polls until cond holds for the plugin's status.
func waitStatus(t *testing.T, h *Host, id string, cond func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, ok := h.Status(id)
		if ok && cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: status %+v never met the condition", id, st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func inState(want State) func(Status) bool { return func(s Status) bool { return s.State == want } }
