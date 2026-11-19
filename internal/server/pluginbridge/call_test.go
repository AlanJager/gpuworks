//go:build linux || darwin

package pluginbridge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/server/store/pstate"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
)

// sdkSpawner runs each plugin as pluginsdk.Serve in a goroutine over real
// socketpairs, answering for the package the host asked to start, so the
// bridge speaks real gwx/1 to a real SDK.
type sdkSpawner struct {
	handler http.Handler

	mu    sync.Mutex
	procs []*sdkProcess
}

func (s *sdkSpawner) Spawn(_ context.Context, req pluginhost.SpawnRequest) (pluginhost.Process, error) {
	serveCore, servePlugin, err := pipePair()
	if err != nil {
		return nil, err
	}
	hostCore, hostPlugin, err := pipePair()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &sdkProcess{serve: serveCore, host: hostCore, plugin: []net.Conn{servePlugin, hostPlugin}, cancel: cancel, done: make(chan struct{})}
	s.mu.Lock()
	s.procs = append(s.procs, p)
	p.pid = 2000 + len(s.procs)
	s.mu.Unlock()
	cfg := pluginsdk.Config{ID: req.Spec.ID, Version: req.Spec.Version, ManifestSHA256: req.Spec.ManifestSHA256,
		Handler: s.handler, Logger: slog.New(slog.DiscardHandler)}
	go func() {
		err := pluginsdk.Serve(ctx, cfg, servePlugin, hostPlugin)
		code := 0
		if err != nil {
			code = 1
		}
		p.finish(pluginhost.ExitStatus{Code: code})
	}()
	return p, nil
}

func (s *sdkSpawner) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.procs)
}

func pipePair() (net.Conn, net.Conn, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	var conns [2]net.Conn
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "gwx-test")
		c, err := net.FileConn(f)
		_ = f.Close()
		if err != nil {
			return nil, nil, err
		}
		conns[i] = c
	}
	return conns[0], conns[1], nil
}

type sdkProcess struct {
	pid         int
	serve, host net.Conn
	plugin      []net.Conn
	cancel      context.CancelFunc
	done        chan struct{}
	once        sync.Once
	mu          sync.Mutex
	exit        pluginhost.ExitStatus
}

func (p *sdkProcess) PID() int              { return p.pid }
func (p *sdkProcess) Serve() net.Conn       { return p.serve }
func (p *sdkProcess) Host() net.Conn        { return p.host }
func (p *sdkProcess) Stderr() io.Reader     { return eofReader{} }
func (p *sdkProcess) Done() <-chan struct{} { return p.done }

func (p *sdkProcess) Exit() pluginhost.ExitStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exit
}

func (p *sdkProcess) Signal(sig os.Signal) error {
	if sig == syscall.SIGKILL || sig == syscall.SIGTERM {
		p.finish(pluginhost.ExitStatus{Code: -1, Signal: sig.String()})
	}
	return nil
}

func (p *sdkProcess) finish(st pluginhost.ExitStatus) {
	p.once.Do(func() {
		p.cancel()
		for _, c := range p.plugin {
			_ = c.Close()
		}
		p.mu.Lock()
		p.exit = st
		p.mu.Unlock()
		close(p.done)
	})
}

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// openWith opens a bridge whose plugins run on sp and runs it; stop ends
// Run and waits for it (also done when the test ends).
func (w *world) openWith(sp pluginhost.Spawner, timing pluginhost.Timing) (b *Bridge, stop func()) {
	w.t.Helper()
	pkgs, err := extpkg.NewHost(extpkg.Setup{
		DataDir: w.dir, Roots: []extapi.PublicKey{w.root.Public}, Store: w.st,
		Config: serverconfig.Plugins{AllowUnsandboxed: []string{"acme_backup", "acme_driver"}},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	b, err = Open(context.Background(), Config{Packages: pkgs, State: pushstate.Config{Store: w.st},
		Features: featuregate.NewDynamicSet(nil), Host: pluginhost.Config{Spawner: sp, Timing: timing},
		Core: extapi.CoreInfo{Version: "dev", InstallID: "inst_bridge_test", OS: "linux", Arch: "amd64", HARole: extapi.HARoleSingle},
		Log:  slog.New(slog.NewTextHandler(&w.logs, nil))})
	if err != nil {
		w.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	stop = sync.OnceFunc(func() {
		cancel()
		if err := <-done; err != nil {
			w.t.Errorf("run: %v", err)
		}
	})
	w.t.Cleanup(stop)
	waitBridgeRunning(w.t, b)
	return b, stop
}

func callTiming() pluginhost.Timing {
	return pluginhost.Timing{HealthEvery: time.Hour, BackoffMin: 10 * time.Millisecond, BackoffMax: 20 * time.Millisecond,
		StopGrace: 300 * time.Millisecond, TermGrace: 100 * time.Millisecond, IdleStop: 50 * time.Millisecond, CallTimeout: 5 * time.Second}
}

func onDemand(s *extsign.Spec) { s.Manifest.Runtime.Lifecycle = extapi.LifecycleOnDemand }

func get(t *testing.T, b *Bridge, id string) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, "/r/x", nil)
	// The host applies callTiming's deadline and retains it until Body.Close.
	return b.Call(context.Background(), id, req, pluginhost.Class{})
}

func waitState(t *testing.T, b *Bridge, id, state string) View {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v := b.view(id)
		if v.State == state {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s stayed %s, want %s", id, v.State, state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func unavailableState(t *testing.T, err error) string {
	t.Helper()
	var pe *messages.ProductError
	if !errors.As(err, &pe) || pe.Code() != messages.ErrorPluginUnavailable || pe.Params()["plugin"] == nil {
		t.Fatalf("err = %v, want PLUGIN_UNAVAILABLE", err)
	}
	s, _ := pe.Params()["state"].(string)
	return s
}

func TestOnDemandCallKeepsItsBodyReadableAfterHeaders(t *testing.T) {
	w := newWorld(t, nil)
	bodyReady := make(chan struct{})
	requestStarted := make(chan context.Context, 1)
	sp := &sdkSpawner{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestStarted <- r.Context()
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-bodyReady:
			_, _ = io.WriteString(w, "ok")
		case <-r.Context().Done():
		}
	})}
	timing := callTiming()
	b, _ := w.openWith(sp, timing)
	install(t, b, w.pkg("acme_driver", "1.0.0", onDemand), true)
	resp, err := get(t, b, "acme_driver")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	requestCtx := <-requestStarted
	select {
	case <-requestCtx.Done():
		t.Fatalf("request canceled with its response body still pending: %v", requestCtx.Err())
	case <-time.After(2 * timing.IdleStop):
	}
	close(bodyReady)
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "ok" {
		t.Fatalf("body after headers = %q, err = %v", body, err)
	}
	if v := b.view("acme_driver"); v.State != string(pluginhost.StateRunning) {
		t.Fatalf("state %s with the response body still open", v.State)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "acme_driver", StateIdle)
}

// An enabled on-demand package is idle until called, runs for the call,
// and goes back to idle when idle long enough; five such cycles leave no
// down record behind, because an on-demand plugin is never reported down.
func TestOnDemandPackageStartsOnCallAndIdlesWithoutDownRecords(t *testing.T) {
	w := newWorld(t, nil)
	sp := &sdkSpawner{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })}
	b, stop := w.openWith(sp, callTiming())
	install(t, b, w.pkg("acme_driver", "1.0.0", onDemand), true)
	waitState(t, b, "acme_driver", StateIdle)
	if sp.count() != 0 {
		t.Fatal("an on-demand package started before it was called")
	}
	for n := 1; n <= 5; n++ {
		resp, err := get(t, b, "acme_driver")
		if err != nil {
			t.Fatalf("cycle %d: %v", n, err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("cycle %d: reading body: %v", n, err)
		}
		if string(body) != "ok" || sp.count() != n {
			t.Fatalf("cycle %d: body %q after %d spawns", n, body, sp.count())
		}
		if v := b.view("acme_driver"); v.State != string(pluginhost.StateRunning) {
			t.Fatalf("cycle %d: state %s while serving", n, v.State)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("cycle %d: closing body: %v", n, err)
		}
		waitState(t, b, "acme_driver", StateIdle)
	}
	stop() // pushed state persists every down record before Run returns
	var rows int64
	if err := w.st.DB().Model(&pstate.Down{}).Count(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("%d down records after five idle cycles and a stop", rows)
	}
}

// A package that is not installed, disabled or broken is refused before
// anything starts.
func TestCallRefusesWhatMayNotRunWithoutStartingIt(t *testing.T) {
	w := newWorld(t, nil)
	sp := &sdkSpawner{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})}
	b, _ := w.openWith(sp, callTiming())
	if _, err := get(t, b, "acme_none"); !isCode(err, messages.ErrorPluginNotInstalled) {
		t.Fatalf("call to a package not installed = %v", err)
	}
	install(t, b, w.pkg("acme_driver", "1.0.0", onDemand), false)
	if _, err := get(t, b, "acme_driver"); unavailableState(t, err) != StateDisabled {
		t.Fatalf("call to a disabled package = %v", err)
	}
	if _, err := b.Enable(context.Background(), "acme_driver", "admin"); err != nil {
		t.Fatal(err)
	}
	// Break the installed binary: the package no longer verifies.
	e, _ := b.entry("acme_driver")
	exe := filepath.Join(b.pkgs.Dir.Version("acme_driver", e.Version), filepath.FromSlash(e.Verified.Binary))
	if err := os.Chmod(exe, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := b.refresh(); err != nil {
		t.Fatal(err)
	}
	if _, err := get(t, b, "acme_driver"); unavailableState(t, err) != StateBroken {
		t.Fatalf("call to a broken package = %v", err)
	}
	if sp.count() != 0 {
		t.Fatalf("%d spawns for packages that may not run", sp.count())
	}
}

// Disabling a package during a call ends the call as failed after
// dispatch, and the next call is refused without starting the plugin again.
func TestDisableDuringACall(t *testing.T) {
	w := newWorld(t, nil)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	defer close(release)
	sp := &sdkSpawner{handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})}
	b, _ := w.openWith(sp, callTiming())
	install(t, b, w.pkg("acme_driver", "1.0.0", onDemand), true)
	waitState(t, b, "acme_driver", StateIdle)
	called := make(chan error, 1)
	go func() {
		resp, err := get(t, b, "acme_driver")
		if err == nil {
			_, err = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		called <- err
	}()
	<-entered
	if _, err := b.Disable(context.Background(), "acme_driver", "admin"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-called:
		// The plugin had the request, so the call may have been partly
		// applied.
		if !isCode(err, messages.ErrorPluginRouteFailed) {
			t.Fatalf("call in flight at the disable = %v, want PLUGIN_ROUTE_FAILED", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call outlived the disable")
	}
	if _, err := get(t, b, "acme_driver"); unavailableState(t, err) != StateDisabled {
		t.Fatalf("call after the disable = %v", err)
	}
	if sp.count() != 1 {
		t.Fatalf("%d spawns, want the one the first call made", sp.count())
	}
}
