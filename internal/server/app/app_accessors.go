package app

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Store exposes the store (used by CLI subcommands like migrate).
func (a *App) Store() *store.Store { return a.store }

// Hub exposes the agent hub. Test harnesses use it to assert connection-level
// facts (e.g. per-node data-lane attach state) that have no REST surface.
func (a *App) Hub() *hub.Hub { return a.hub }

// SetControllerRetryPolicy is intentionally small and test-oriented: L1
// harnesses need to exercise bounded failure recovery without waiting for the
// production 15-second backoff. It does not alter the default configuration.
func (a *App) SetControllerRetryPolicy(backoff time.Duration, maxAttempts int) {
	a.ctrl.SetRetryPolicy(backoff, maxAttempts)
}

// SetControllerStartTimeout shortens the agent health budget for integration
// tests; normal applications retain the controller's production default.
func (a *App) SetControllerStartTimeout(seconds int) {
	a.ctrl.SetStartTimeout(seconds)
}

// SetControllerEnginePorts assigns the host-port range used for engine
// instances. It is intended for hermetic test harnesses; production callers
// should retain the controller's default range.
func (a *App) SetControllerEnginePorts(start, end int) {
	a.ctrl.SetEnginePorts(start, end)
}

// DialServerAgent establishes an authenticated server_dial session. It is
// used by integration harnesses and follows the same hub path as the API.
func (a *App) DialServerAgent(ctx context.Context, addr, token string) error {
	return a.hub.DialAgentWithSecret(ctx, addr, token)
}

// LoopbackURL is the base URL this server answers on over 127.0.0.1, for a
// caller in the same process that has to reach it the way a client would.
func (a *App) LoopbackURL() string { return a.serverSettings().LoopbackURL() }
