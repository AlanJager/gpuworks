package pluginhost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginexec"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Host supervises every plugin the server runs.
type Host struct {
	cfg     Config
	timing  Timing
	spawner Spawner
	log     *slog.Logger

	// generation numbers every process the host starts, across plugins
	// and restarts, so a later process always has a higher number.
	generation atomic.Uint64

	mu      sync.Mutex
	plugins map[string]*instance
	closed  bool
}

// New returns a host that runs nothing until Start.
func New(cfg Config) (*Host, error) {
	if cfg.Core == nil {
		return nil, errors.New("pluginhost: Config.Core is required")
	}
	h := &Host{cfg: cfg, timing: cfg.Timing.withDefaults(), spawner: cfg.Spawner, log: cfg.Log, plugins: map[string]*instance{}}
	if h.log == nil {
		h.log = slog.Default()
	}
	if h.spawner == nil {
		path := cfg.Trampoline
		if path == "" {
			exe, err := os.Executable()
			if err != nil {
				return nil, fmt.Errorf("pluginhost: locating the server binary for the trampoline: %w", err)
			}
			path = exe
		}
		h.spawner = trampolineSpawner{path: path}
	}
	if h.cfg.Environ == nil {
		h.cfg.Environ = os.Environ()
	}
	if h.cfg.Memory == nil {
		h.cfg.Memory = defaultMemoryReader()
	}
	return h, nil
}

// Start supervises spec's plugin: a resident plugin is started now, an
// on-demand one on its first call. Start does not wait for it to run.
func (h *Host) Start(spec Spec) error {
	if err := spec.validate(); err != nil {
		return err
	}
	spec = spec.clone()
	if spec.MaxConcurrency == 0 {
		spec.MaxConcurrency = DefaultMaxConcurrency
	}
	if spec.MemMiB == 0 {
		spec.MemMiB = DefaultMemMiB
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return errors.New("pluginhost: the host is closed")
	}
	if i, ok := h.plugins[spec.ID]; ok && !i.finished() {
		return fmt.Errorf("pluginhost: %s is already supervised", spec.ID)
	}
	i := newInstance(h, spec)
	h.plugins[spec.ID] = i
	go i.supervise()
	return nil
}

// Stop stops a plugin gracefully (shutdown, then SIGTERM, then SIGKILL to
// its process group) and waits until it has exited or ctx ends. Its
// status stays readable as stopped. The plugin's cgroup directory is
// removed with it, also when a previous removal failed or the plugin is
// not supervised now, so stopping before an uninstall leaves none behind.
func (h *Host) Stop(ctx context.Context, id string) error {
	i := h.get(id)
	if i == nil {
		h.removeCgroup(id)
		return ErrUnknownPlugin
	}
	i.requestStop()
	select {
	case <-i.done:
		h.removeCgroup(id)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// removeCgroup removes package id's cgroup directory, if plugins get
// cgroups here and the directory exists.
func (h *Host) removeCgroup(id string) {
	c := h.cgroup(0)
	if c == nil || !extapi.ValidPackageID(id) {
		return
	}
	if err := pluginexec.RemoveCgroup(c, id); err != nil {
		h.log.Warn("plugin cgroup not removed", "plugin", id, "err", err)
	}
}

// cgroup is where plugins with memMiB get their cgroups; nil when the
// server places none.
func (h *Host) cgroup(memMiB int) *pluginexec.Cgroup {
	if h.cfg.CgroupRoot == "" {
		return nil
	}
	mem := int64(memMiB) << 20
	return &pluginexec.Cgroup{Root: h.cfg.CgroupRoot, Parent: h.cfg.CgroupParent, MemoryMax: mem, MemoryHigh: mem / 10 * 9,
		CPUWeight: pluginCPUWeight, PidsMax: pluginPidsMax}
}

// Close stops every plugin, removes their cgroup directories and refuses
// further starts.
func (h *Host) Close(ctx context.Context) error {
	h.mu.Lock()
	h.closed = true
	all := slices.Collect(maps.Values(h.plugins))
	h.mu.Unlock()
	for _, i := range all {
		i.requestStop()
	}
	for _, i := range all {
		select {
		case <-i.done:
			h.removeCgroup(i.spec.ID)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Status returns a plugin's status.
func (h *Host) Status(id string) (Status, bool) {
	i := h.get(id)
	if i == nil {
		return Status{}, false
	}
	return i.snapshot(), true
}

// List returns every supervised plugin's status, sorted by id.
func (h *Host) List() []Status {
	h.mu.Lock()
	all := slices.Collect(maps.Values(h.plugins))
	h.mu.Unlock()
	out := make([]Status, 0, len(all))
	for _, i := range all {
		out = append(out, i.snapshot())
	}
	slices.SortFunc(out, func(a, b Status) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return out
}

// Session returns what the server needs of a running plugin.
func (h *Host) Session(id string) (Session, bool) {
	i := h.get(id)
	if i == nil {
		return Session{}, false
	}
	s := i.currentRun()
	if s == nil {
		return Session{}, false
	}
	return s.session(), true
}

// Stderr returns the plugin's recent stderr output (up to 1 MiB, across
// restarts).
func (h *Host) Stderr(id string) []byte {
	i := h.get(id)
	if i == nil {
		return nil
	}
	return i.stderr.Bytes()
}

// Class is how a call counts against a plugin's slots; the caller says.
type Class struct {
	// Admin marks a call made for an administrator (a route of access
	// admin or admin_session): it may take the slots reserved from member
	// calls (AdminReserve), so members cannot starve an administrator.
	Admin bool
	// Stream marks a call whose response streams: it holds its slot until
	// the body is closed and takes it from a pool of its own, so open
	// streams never block short calls.
	Stream bool
}

// Do sends req to the plugin (a route or a service; only the method, path,
// headers and body matter) within ctx's deadline, or CallTimeout without
// one; an on-demand plugin is started first and its start counts against
// that time. A refused call is a *messages.ProductError:
//
//   - PLUGIN_UNAVAILABLE{plugin, state}: the call never reached the plugin
//     (not supervised, not running, crashlooping, or the connection failed
//     before the request headers were written), so it is safe to retry;
//   - PLUGIN_BUSY{id, limit}: every slot the call's class may take is in
//     use;
//   - PLUGIN_TIMEOUT{id, timeout_s, state}: the deadline passed while the
//     plugin was starting (state starting) or answering (state running);
//   - PLUGIN_ROUTE_FAILED{id}: the plugin had the request and the call
//     failed, so it may have been partly applied; not retryable.
//
// A plugin's own refusal is a normal response.
func (h *Host) Do(ctx context.Context, id string, req *http.Request, class Class) (*http.Response, error) {
	i := h.get(id)
	if i == nil {
		return nil, Unavailable(id, string(StateStopped), ErrUnknownPlugin)
	}
	timeout := h.timing.CallTimeout
	if d, ok := ctx.Deadline(); ok {
		timeout = time.Until(d)
	}
	deadline := time.Now().Add(timeout)
	wait, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for {
		r, err := i.runFor(wait)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, timedOut(id, timeout, StateStarting, err)
			}
			var pe *messages.ProductError
			if errors.As(err, &pe) {
				return nil, err
			}
			return nil, Unavailable(id, string(i.snapshot().State), err)
		}
		resp, err := r.conn.do(ctx, req, class, deadline, r.touch)
		var busy *busyError
		var failed *callError
		switch {
		case err == nil:
			return resp, nil
		case errors.Is(err, errDraining):
			continue // stopping while idle; runFor waits for the next start
		case errors.As(err, &busy):
			return nil, messages.NewProductError(messages.ErrorPluginBusy, "the plugin is busy",
				map[string]any{"id": id, "limit": busy.limit})
		case errors.Is(err, context.DeadlineExceeded):
			return nil, timedOut(id, timeout, StateRunning, err)
		case errors.As(err, &failed) && failed.dispatched:
			return nil, messages.WrapProductError(messages.ErrorPluginRouteFailed,
				"the plugin failed while handling the request; it may have been partly applied", map[string]any{"id": id}, err)
		default:
			return nil, Unavailable(id, string(i.snapshot().State), err)
		}
	}
}

// Unavailable is the refusal of a call that never reached plugin id,
// which is in state (a State, or a package state the caller knows).
func Unavailable(id, state string, cause error) error {
	return messages.WrapProductError(messages.ErrorPluginUnavailable, "the plugin is not running",
		map[string]any{"plugin": id, "state": state}, cause)
}

func timedOut(id string, timeout time.Duration, state State, cause error) error {
	secs := int((timeout + time.Second - 1) / time.Second)
	return messages.WrapProductError(messages.ErrorPluginTimeout, "the plugin did not answer in time",
		map[string]any{"id": id, "timeout_s": max(secs, 1), "state": string(state)}, cause)
}

func (h *Host) get(id string) *instance {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.plugins[id]
}

// notify calls OnState with a copy of st.
func (h *Host) notify(st Status) {
	if h.cfg.OnState != nil {
		h.cfg.OnState(st.clone())
	}
}

// backoff is the delay before restart n (1-based) of a crashing plugin.
func (t Timing) backoff(n int) time.Duration {
	d := t.BackoffMin
	for k := 1; k < n && d < t.BackoffMax; k++ {
		d *= 2
	}
	return min(d, t.BackoffMax)
}
