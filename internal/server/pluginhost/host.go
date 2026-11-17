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
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Host supervises every plugin the server runs.
type Host struct {
	cfg     Config
	timing  Timing
	spawner Spawner
	log     *slog.Logger

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
// status stays readable as stopped.
func (h *Host) Stop(ctx context.Context, id string) error {
	i := h.get(id)
	if i == nil {
		return ErrUnknownPlugin
	}
	i.requestStop()
	select {
	case <-i.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops every plugin and refuses further starts.
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

// Do sends req to the plugin (a route or a service; only the method, path,
// headers and body matter) with ctx's deadline. An on-demand plugin is
// started first. It fails with a *messages.ProductError when the plugin is
// not running (PLUGIN_UNAVAILABLE) or every call slot is taken
// (PLUGIN_BUSY); a plugin's own refusal is a normal response.
func (h *Host) Do(ctx context.Context, id string, req *http.Request) (*http.Response, error) {
	i := h.get(id)
	if i == nil {
		return nil, unavailable(id, ErrUnknownPlugin)
	}
	r, err := i.runFor(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := r.conn.do(ctx, req)
	r.touch()
	switch {
	case errors.Is(err, errBusy):
		return nil, messages.NewProductError(messages.ErrorPluginBusy, "the plugin is busy",
			map[string]any{"id": id, "limit": i.spec.MaxConcurrency})
	case err != nil:
		return nil, unavailable(id, err)
	}
	return resp, nil
}

func unavailable(id string, cause error) error {
	return messages.WrapProductError(messages.ErrorPluginUnavailable, "the plugin is not running", map[string]any{"plugin": id}, cause)
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
