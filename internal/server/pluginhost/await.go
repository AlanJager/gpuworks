package pluginhost

import (
	"context"
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// UnhealthyError is AwaitHealthy's answer when the plugin did not stay up:
// the status it was in when the wait gave up.
type UnhealthyError struct {
	Status Status
	// Timeout is set when the wait ended (its context) before the plugin
	// came up.
	Timeout bool
}

func (e *UnhealthyError) Error() string {
	if e.Timeout {
		return fmt.Sprintf("plugin %s did not come up in time (%s)", e.Status.ID, e.Status.State)
	}
	return fmt.Sprintf("plugin %s did not stay up: %s %s", e.Status.ID, e.Status.State, e.Status.Code)
}

// Code is the code that says why: the status code of the failure, else
// PLUGIN_TIMEOUT for a wait that ended or PLUGIN_UNAVAILABLE.
func (e *UnhealthyError) Code() string {
	switch {
	case e.Status.Code != "":
		return e.Status.Code
	case e.Timeout:
		return messages.ErrorPluginTimeout
	}
	return messages.ErrorPluginUnavailable
}

// AwaitHealthy waits until plugin id has come up (running or degraded)
// and stayed up, in one process, until hold has passed since the call; it
// is the health gate of an upgrade. It fails at once, with an
// *UnhealthyError, when the plugin crashes, is refused, fails or stops,
// and when it comes up again as another process; and when ctx ends first.
// An on-demand plugin is asked to start once (as its first call would);
// it must come up, and may then go idle and start again for later calls.
// A plugin that is not supervised is ErrUnknownPlugin.
func (h *Host) AwaitHealthy(ctx context.Context, id string, hold time.Duration) error {
	i := h.get(id)
	if i == nil {
		return ErrUnknownPlugin
	}
	onDemand := i.spec.Lifecycle == extapi.LifecycleOnDemand
	if onDemand {
		select {
		case i.wake <- struct{}{}:
		default:
		}
	}
	timer := time.NewTimer(hold)
	defer timer.Stop()
	held := false
	var upGeneration uint64
	for {
		i.mu.Lock()
		st, changed := i.status.clone(), i.changed
		i.mu.Unlock()
		switch {
		case st.State.Up():
			if upGeneration == 0 {
				upGeneration = st.Generation
			} else if st.Generation != upGeneration && !onDemand {
				// A resident plugin up as another process crashed
				// between two looks at its status.
				return &UnhealthyError{Status: st}
			}
		case st.State == StateStarting && (upGeneration == 0 || onDemand),
			(st.State == StateIdle || st.State == StateStopping) && onDemand && upGeneration != 0,
			st.State == StateIdle && onDemand:
			// Coming up; or an on-demand plugin between its calls.
		default:
			return &UnhealthyError{Status: st}
		}
		if held && upGeneration != 0 {
			return nil
		}
		select {
		case <-changed:
		case <-timer.C:
			held = true
		case <-ctx.Done():
			return &UnhealthyError{Status: st, Timeout: true}
		}
	}
}
