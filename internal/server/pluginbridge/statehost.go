package pluginbridge

import (
	"context"
	"errors"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// stateHost applies pushes from plugin host connections to pushed state.
type stateHost struct{ b *Bridge }

// Held is what the core holds for package id.
func (h stateHost) Held(id string) []extapi.HeldState { return h.b.state.Held(id) }

// Push applies a push as the package bound to the connection it arrived
// on: the identity is the verified package the host started on that
// connection (its certificate's grants, its approved permissions, its
// codes), never anything in the push, and the connection's process
// generation lets pushed state refuse a replaced process. A connection of
// a package started under another version, or no longer started, is
// refused as replaced.
func (h stateHost) Push(ctx context.Context, owner pluginhost.Owner, method string, push *extapi.StatePush) (*extapi.StateAck, error) {
	h.b.mu.Lock()
	id, ok := h.b.pushers[owner.ID]
	h.b.mu.Unlock()
	if !ok || id.version != owner.Version {
		return nil, pluginhost.Refuse(extapi.ReasonFailedPrecondition, messages.ErrorPluginStateConnectionReplaced,
			"the plugin is no longer started under this connection's package",
			map[string]any{"id": owner.ID, "kind": push.Kind})
	}
	if method == http.MethodPatch {
		return nil, pluginhost.Refuse(extapi.ReasonUnsupported, messages.ErrorPluginHostRouteUnknown,
			"no pushed table of this server takes a patch", map[string]any{"method": method, "path": extapi.PathHostState + push.Kind})
	}
	pusher := id.pusher
	pusher.Generation = owner.Generation
	ack, werr := h.b.state.Push(ctx, pusher, push)
	if werr != nil {
		return nil, &pluginhost.Error{Status: extapi.StatusForReason(werr.Reason), Wire: *werr}
	}
	return ack, nil
}

// Clocks reads pushed state's clocks for the host API's meta.
func (h stateHost) Clocks() (wall, ext, hwm time.Time) {
	r := h.b.state.Clocks()
	return r.Wall, r.ExtClock, r.HWM
}

// Correct moves extclock for a package whose clock_authority role the host
// API checked, through the same bounded, audited path as every correction.
func (h stateHost) Correct(ctx context.Context, owner pluginhost.Owner, to time.Time, reason string) (time.Time, time.Time, error) {
	from := h.b.state.Now()
	if err := h.b.state.CorrectClock(ctx, to, owner.ID); err != nil {
		var pe *messages.ProductError
		if errors.As(err, &pe) {
			return time.Time{}, time.Time{}, pluginhost.Refuse(extapi.ReasonInvalidArgument, pe.Code(), pe.Error(), pe.Params())
		}
		return time.Time{}, time.Time{}, err
	}
	h.b.log.Info("a plugin corrected extclock", "audit", true, "plugin", owner.ID, "version", owner.Version,
		"from", from.Format(time.RFC3339), "to", to.UTC().Format(time.RFC3339), "reason", reason)
	return from, to, nil
}

// Ready ends the boot extension of the plugin process the connection
// belongs to.
func (h stateHost) Ready(owner pluginhost.Owner) { h.b.state.Ready(owner.ID, owner.Generation) }

// OwnRows reads back the connection's own package's table of kind.
func (h stateHost) OwnRows(owner pluginhost.Owner, kind string) (extapi.PushedState, error) {
	return h.b.state.OwnerRows(owner.ID, kind)
}
