package pluginhost

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/http2"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginexec"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// The sandbox every plugin gets (docs/dev/extensions.md).
const (
	pluginNice      = 5
	pluginNoFile    = 4096
	pluginCPUWeight = 50
	pluginPidsMax   = 512
	// hostStreams bounds the plugin's concurrent host API calls.
	hostStreams = 8
	// stderrDrain bounds the wait for a dead plugin's stderr to close, and
	// exitWait the wait for the exit of a plugin whose connection broke.
	stderrDrain = 2 * time.Second
	exitWait    = time.Second
)

// landlockABI is pluginexec.LandlockABI; tests replace it.
var landlockABI = pluginexec.LandlockABI

// systemReadPaths are what a sandboxed plugin may read outside its own
// directories: CA certificates, the resolver's files and time zones.
var systemReadPaths = []string{
	"/etc/ssl", "/etc/pki", "/etc/ca-certificates", "/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf",
	"/etc/localtime", "/usr/share/zoneinfo",
}

// procRun is one accepted process: its client connection, its handshake
// and its health.
type procRun struct {
	conn     *pluginConn
	resp     *extapi.HandshakeResponse
	key      []byte
	lastCall atomic.Int64

	mu     sync.Mutex
	health extapi.Health
}

func (r *procRun) touch() { r.lastCall.Store(time.Now().UnixNano()) }

func (r *procRun) session() Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Session{PrincipalKey: append([]byte(nil), r.key...), Use: cloneUse(r.resp.Use),
		Services: append([]string(nil), r.resp.Services...), Health: r.health}
}

func cloneUse(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (i *instance) profile() *pluginexec.Profile {
	s := &i.spec
	p := &pluginexec.Profile{ID: s.ID, ParentPID: os.Getpid(), Dir: s.DataDir, Nice: pluginNice, NoFile: pluginNoFile,
		Landlock: &pluginexec.Landlock{ReadExec: []string{s.PackageDir}, ReadWrite: []string{s.DataDir, s.TmpDir},
			Read: systemReadPaths, Devices: s.Devices},
		RequireLandlock: s.RequireLandlock, Cgroup: i.h.cgroup(s.MemMiB)}
	return p
}

// runOnce starts the plugin, completes the handshake and serves it until it
// exits, is stopped, goes idle or fails its health checks. upSince is when
// the handshake was accepted (zero if it never was).
func (i *instance) runOnce() (o outcome, upSince time.Time) {
	h, spec := i.h, &i.spec
	isolation := "none"
	if landlockABI() > 0 {
		isolation = "landlock"
	}
	gen := h.generation.Add(1)
	i.update(func(s *Status) {
		s.State, s.Code, s.Params, s.PID, s.Use, s.Isolation = StateStarting, "", nil, 0, nil, isolation
		s.Cgroup, s.RSSBytes = "", 0
		s.Starts++
		s.Generation = gen
	})
	if spec.RequireLandlock && isolation != "landlock" {
		return outcome{kind: outcomeFailed, code: messages.ErrorPluginIsolationUnavailable,
			params: map[string]any{"id": spec.ID, "isolation": "landlock"}}, time.Time{}
	}
	prof := i.profile()
	env := pluginexec.Environ(h.cfg.Environ, spec.DataDir, spec.TmpDir, extapi.ProtocolMajor)
	proc, err := h.spawner.Spawn(context.Background(), SpawnRequest{Spec: spec, Profile: prof, Env: env})
	if err != nil {
		return startFailed(spec.ID, err), time.Time{}
	}
	i.update(func(s *Status) { s.PID = proc.PID() })
	stderrDone := make(chan struct{})
	go func() {
		forwardStderr(proc.Stderr(), spec.ID, h.log, i.stderr, time.Now)
		close(stderrDone)
	}()
	hc := newHostConn(spec, h.cfg.State, h.cfg.Reads, h.log)
	hc.svc = h.svc
	go (&http2.Server{MaxConcurrentStreams: hostStreams}).ServeConn(proc.Host(), &http2.ServeConnOpts{Handler: hc})
	conn, connErr := newPluginConn(proc.Serve(), spec.MaxConcurrency, h.timing.CallTimeout)
	defer func() {
		hc.reject()
		if conn != nil {
			conn.close()
		}
		_ = proc.Host().Close()
		if !isDone(proc) {
			_ = proc.Signal(syscall.SIGKILL)
			<-proc.Done()
		}
		select {
		case <-stderrDone:
		case <-time.After(stderrDrain):
		}
		if prof.Cgroup != nil {
			_ = pluginexec.RemoveCgroup(prof.Cgroup, spec.ID)
		}
		i.update(func(s *Status) { s.Cgroup, s.RSSBytes = "", 0 })
	}()
	if connErr != nil {
		conn = nil
		return startFailed(spec.ID, connErr), time.Time{}
	}
	req, err := h.handshakeRequest(spec)
	if err != nil {
		return outcome{kind: outcomeFailed, code: messages.ErrorPluginStartFailed, params: map[string]any{"id": spec.ID, "detail": err.Error()}}, time.Time{}
	}
	type answer struct {
		resp *extapi.HandshakeResponse
		o    *outcome
	}
	answered := make(chan answer, 1)
	hctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		resp, o := h.handshake(hctx, spec, conn, req)
		answered <- answer{resp, o}
	}()
	var resp *extapi.HandshakeResponse
	select {
	case a := <-answered:
		if a.o != nil && a.o.connBroken && waitDone(proc, exitWait) {
			return exitOutcome(spec.ID, proc.Exit(), true), time.Time{}
		}
		if a.o != nil {
			return *a.o, time.Time{}
		}
		resp = a.resp
	case <-proc.Done():
		return exitOutcome(spec.ID, proc.Exit(), true), time.Time{}
	case <-i.stop:
		i.set(StateStopping, "", nil)
		i.terminate(proc, conn)
		return outcome{kind: outcomeStopped}, time.Time{}
	}
	hc.accept(Owner{ID: spec.ID, Version: spec.Version, Lifecycle: spec.Lifecycle, Grant: spec.Grant, Use: cloneUse(resp.Use), Generation: gen})
	run := &procRun{conn: conn, resp: resp, key: req.PrincipalKey, health: extapi.Health{Status: extapi.HealthOK}}
	run.touch()
	upSince = time.Now()
	i.mu.Lock()
	i.run = run
	i.mu.Unlock()
	// The trampoline has executed the plugin by now, so its cgroup move
	// (best effort) is settled either way.
	cgroup := ""
	if prof.Cgroup != nil && pluginexec.Placed(prof.Cgroup, spec.ID, proc.PID()) {
		cgroup = path.Join(prof.Cgroup.Parent, "plugins", spec.ID)
	}
	i.update(func(s *Status) { s.State, s.Use, s.Cgroup = StateRunning, cloneUse(resp.Use), cgroup })
	defer func() {
		i.mu.Lock()
		i.run = nil
		i.mu.Unlock()
	}()
	var mem *memoryWatch
	if cgroup == "" {
		mem = &memoryWatch{reader: h.cfg.Memory}
	}
	return i.serve(proc, run, mem), upSince
}

// serve watches an accepted process: its exit, a stop request, its health,
// its memory when mem is set (the plugin has no cgroup of its own) and,
// for an on-demand plugin, idleness.
func (i *instance) serve(proc Process, run *procRun, mem *memoryWatch) outcome {
	t, spec := i.h.timing, &i.spec
	health := time.NewTicker(t.HealthEvery)
	defer health.Stop()
	var memTick <-chan time.Time
	if mem != nil {
		tick := time.NewTicker(t.MemoryEvery)
		defer tick.Stop()
		memTick = tick.C
	}
	var idle <-chan time.Time
	if spec.Lifecycle == extapi.LifecycleOnDemand {
		tick := time.NewTicker(max(t.IdleStop/4, 10*time.Millisecond))
		defer tick.Stop()
		idle = tick.C
	}
	misses := 0
	for {
		select {
		case <-proc.Done():
			return exitOutcome(spec.ID, proc.Exit(), false)
		case <-i.stop:
			i.set(StateStopping, "", nil)
			i.terminate(proc, run.conn)
			return outcome{kind: outcomeStopped}
		case <-memTick:
			if o, killed := i.checkMemory(proc, mem); killed {
				return o
			}
		case <-idle:
			if time.Since(time.Unix(0, run.lastCall.Load())) < t.IdleStop {
				continue
			}
			// Refusing new calls and clearing the run is one step under
			// the lock, so a call either got its slot before (and the
			// plugin stays) or waits for the next start.
			i.mu.Lock()
			drained := run.conn.drainIfIdle()
			if drained {
				i.run, i.idling = nil, true
			}
			i.mu.Unlock()
			if drained {
				i.set(StateStopping, "", nil)
				i.terminate(proc, run.conn)
				return outcome{kind: outcomeIdle}
			}
		case <-health.C:
			hres, err := i.probe(run.conn)
			if err != nil {
				misses++
				i.h.log.Warn("plugin health probe failed", "plugin", spec.ID, "misses", misses, "err", err)
				if misses >= t.HealthMisses {
					_ = proc.Signal(syscall.SIGKILL)
					<-proc.Done()
					return outcome{kind: outcomeCrash, code: messages.ErrorPluginUnhealthy, params: map[string]any{"id": spec.ID, "misses": misses}}
				}
				continue
			}
			misses = 0
			run.mu.Lock()
			run.health = *hres
			run.mu.Unlock()
			i.update(func(s *Status) {
				if hres.Status == extapi.HealthDegraded {
					s.State, s.Code, s.Params = StateDegraded, hres.Code, maps.Clone(map[string]any(hres.Params))
				} else {
					s.State, s.Code, s.Params = StateRunning, "", nil
				}
			})
		}
	}
}

func (i *instance) probe(c *pluginConn) (*extapi.Health, error) {
	raw, err := c.lifecycle(context.Background(), http.MethodGet, extapi.PathHealth, nil, i.h.timing.HealthTimeout)
	if err != nil {
		return nil, err
	}
	return extapi.DecodeHealth(raw)
}

// terminate stops a process gracefully: shutdown, StopGrace, SIGTERM to the
// group, TermGrace, SIGKILL.
func (i *instance) terminate(proc Process, c *pluginConn) {
	t := i.h.timing
	deadline := time.Now().Add(t.StopGrace)
	if c != nil {
		_, _ = c.lifecycle(context.Background(), http.MethodPost, extapi.PathShutdown, nil, t.StopGrace)
		// The answer is in; hanging up lets the plugin finish draining at
		// once instead of waiting out its HTTP/2 goodbye.
		c.close()
	}
	if waitDone(proc, time.Until(deadline)) {
		return
	}
	_ = proc.Signal(syscall.SIGTERM)
	if waitDone(proc, t.TermGrace) {
		return
	}
	_ = proc.Signal(syscall.SIGKILL)
	<-proc.Done()
}

func waitDone(proc Process, d time.Duration) bool {
	if d <= 0 {
		return isDone(proc)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-proc.Done():
		return true
	case <-timer.C:
		return false
	}
}

func isDone(proc Process) bool {
	select {
	case <-proc.Done():
		return true
	default:
		return false
	}
}

func startFailed(id string, err error) outcome {
	return outcome{kind: outcomeCrash, code: messages.ErrorPluginStartFailed, params: map[string]any{"id": id, "detail": err.Error()}}
}

// exitOutcome reads a process exit. Before the handshake, the trampoline's
// own exit codes say why the plugin never ran.
func exitOutcome(id string, exit ExitStatus, beforeHandshake bool) outcome {
	if beforeHandshake {
		switch exit.Code {
		case pluginexec.ExitIsolationUnavailable:
			return outcome{kind: outcomeFailed, code: messages.ErrorPluginIsolationUnavailable, params: map[string]any{"id": id, "isolation": "landlock"}}
		case pluginexec.ExitUsage:
			return outcome{kind: outcomeFailed, code: messages.ErrorPluginStartFailed,
				params: map[string]any{"id": id, "detail": "the trampoline refused its command line"}}
		case pluginexec.ExitSetupFailed, pluginexec.ExitExecFailed:
			return outcome{kind: outcomeCrash, code: messages.ErrorPluginStartFailed,
				params: map[string]any{"id": id, "detail": fmt.Sprintf("the trampoline exited with %d", exit.Code)}}
		}
	}
	if exit.Signal != "" {
		return outcome{kind: outcomeCrash, code: messages.ErrorPluginKilled, params: map[string]any{"id": id, "signal": exit.Signal}}
	}
	return outcome{kind: outcomeCrash, code: messages.ErrorPluginCrashed, params: map[string]any{"id": id, "exit_code": exit.Code}}
}
