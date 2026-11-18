package pluginbridge

import (
	"context"
	"net/http"

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
