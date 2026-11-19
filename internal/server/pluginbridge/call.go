package pluginbridge

import (
	"context"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/pluginhost"
)

// Call sends req to package id's server part (see pluginhost.Host.Do for
// the request and the refusals), counted as class. Only a package that is
// installed, enabled and verified, with a server part, is called; any
// other is refused before anything starts: PLUGIN_NOT_INSTALLED, or
// PLUGIN_UNAVAILABLE{plugin, state} with the package's state (disabled,
// broken, revoked, installed, or failed when its start was refused). An
// on-demand server part is started by the call.
func (b *Bridge) Call(ctx context.Context, id string, req *http.Request, class pluginhost.Class) (*http.Response, error) {
	e, err := b.entry(id)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	refused := b.refusals[id]
	b.mu.Unlock()
	switch {
	case e.Err != nil || e.Verified == nil:
		return nil, pluginhost.Unavailable(id, StateBroken, e.Err)
	case e.Verified.Revoked != nil:
		return nil, pluginhost.Unavailable(id, StateRevoked, nil)
	case !e.Control.Enabled:
		return nil, pluginhost.Unavailable(id, StateDisabled, nil)
	case !runnable(e):
		return nil, pluginhost.Unavailable(id, StateInstalled, nil)
	case refused != nil:
		return nil, pluginhost.Unavailable(id, string(pluginhost.StateFailed), refused)
	}
	return b.host.Do(ctx, id, req, class)
}
