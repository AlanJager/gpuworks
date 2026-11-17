package pluginhost

import (
	"context"
	"maps"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// instance supervises one package's plugin: its status, its current run
// and its crash history live and die together.
type instance struct {
	h      *Host
	spec   Spec
	stderr *ring

	mu      sync.Mutex
	status  Status
	run     *procRun      // the accepted run, while there is one
	changed chan struct{} // closed and replaced on every status change

	stop     chan struct{}
	stopOnce sync.Once
	wake     chan struct{} // an on-demand call asks for a start
	done     chan struct{} // closed when supervise returns

	// Crash history, touched only by supervise.
	crashes     []time.Time
	consecutive int
}

func newInstance(h *Host, spec Spec) *instance {
	return &instance{h: h, spec: spec, stderr: newRing(stderrRingBytes),
		status:  Status{ID: spec.ID, State: StateStarting, Since: time.Now(), Isolation: "none"},
		changed: make(chan struct{}), stop: make(chan struct{}), wake: make(chan struct{}, 1), done: make(chan struct{})}
}

func (i *instance) requestStop() { i.stopOnce.Do(func() { close(i.stop) }) }

func (i *instance) finished() bool {
	select {
	case <-i.done:
		return true
	default:
		return false
	}
}

func (i *instance) snapshot() Status {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.status.clone()
}

func (i *instance) currentRun() *procRun {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.status.State.Up() {
		return nil
	}
	return i.run
}

// update changes the status under the lock, logs a change of state, and
// tells waiters and OnState.
func (i *instance) update(f func(*Status)) {
	i.mu.Lock()
	before := i.status.State
	f(&i.status)
	if i.status.State != before {
		i.status.Since = time.Now()
	}
	st := i.status.clone()
	close(i.changed)
	i.changed = make(chan struct{})
	i.mu.Unlock()
	if st.State != before {
		level := i.h.log.Info
		switch st.State {
		case StateBackoff, StateCrashloop, StateFailed:
			level = i.h.log.Warn
		}
		level("plugin state", "plugin", st.ID, "state", string(st.State), "from", string(before), "code", st.Code, "params", st.Params)
	}
	i.h.notify(st)
}

func (i *instance) set(state State, code string, params map[string]any) {
	i.update(func(s *Status) { s.State, s.Code, s.Params = state, code, maps.Clone(params) })
}

// supervise runs the plugin until it is stopped: it restarts a crashed
// plugin after a backoff, slowly once it crashloops, and waits for a call
// before starting an on-demand one.
func (i *instance) supervise() {
	defer close(i.done)
	t := i.h.timing
	for {
		if i.spec.Lifecycle == extapi.LifecycleOnDemand {
			i.set(StateIdle, "", nil)
			select {
			case <-i.wake:
			case <-i.stop:
				i.set(StateStopped, "", nil)
				return
			}
		}
		o, upSince := i.runOnce()
		switch o.kind {
		case outcomeStopped:
			i.set(StateStopped, "", nil)
			return
		case outcomeIdle:
			continue
		case outcomeFailed:
			i.set(StateFailed, o.code, o.params)
			<-i.stop
			i.update(func(s *Status) { s.State = StateStopped })
			return
		}
		now := time.Now()
		if !upSince.IsZero() && now.Sub(upSince) >= t.HealthyReset {
			i.consecutive = 0
		}
		i.consecutive++
		kept := i.crashes[:0]
		for _, c := range i.crashes {
			if now.Sub(c) < t.CrashloopWindow {
				kept = append(kept, c)
			}
		}
		i.crashes = append(kept, now)
		delay := t.backoff(i.consecutive)
		if len(i.crashes) >= t.CrashloopCount {
			delay = t.CrashloopRetry
			i.set(StateCrashloop, messages.ErrorPluginCrashloop, map[string]any{"id": i.spec.ID, "crashes": len(i.crashes),
				"window_s": int(t.CrashloopWindow / time.Second), "retry_s": int(t.CrashloopRetry / time.Second)})
		} else {
			i.set(StateBackoff, o.code, o.params)
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-i.stop:
			timer.Stop()
			i.set(StateStopped, "", nil)
			return
		}
	}
}

// runFor returns the running process for a call, starting an on-demand
// plugin and waiting for it within ctx.
func (i *instance) runFor(ctx context.Context) (*procRun, error) {
	for {
		i.mu.Lock()
		r, st, changed := i.run, i.status.State, i.changed
		i.mu.Unlock()
		if r != nil && st.Up() {
			return r, nil
		}
		if i.spec.Lifecycle != extapi.LifecycleOnDemand || st == StateFailed || st == StateStopped || st == StateStopping {
			return nil, unavailable(i.spec.ID, nil)
		}
		if st == StateIdle {
			// Asked only of an idle plugin, so a start already under way
			// leaves no stale request to start it again after it idles.
			select {
			case i.wake <- struct{}{}:
			default:
			}
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, unavailable(i.spec.ID, ctx.Err())
		}
	}
}
