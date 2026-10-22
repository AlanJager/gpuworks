package engine

import "context"

// LogDrain ships what an instance's container wrote before its removal deletes
// that output with it. exited reports that the container had stopped on its
// own before the removal began, rather than being stopped by it. The returned
// release runs once the removal attempt has finished, successful or not.
type LogDrain func(ctx context.Context, uuid string, exited bool) (release func())

// WithLogDrain hands the node's log shipper the last word on a container's
// output. The shipper only follows containers on a periodic tick, so one that
// fails within seconds of starting would otherwise be removed, together with
// the only record of why it failed, before any of it reached the server.
func WithLogDrain(drain LogDrain) EngineOption {
	return func(engine *Engine) {
		engine.logDrain = drain
	}
}

// drainLogs runs the configured drain; without one, removal goes ahead as is.
func (e *Engine) drainLogs(ctx context.Context, uuid string, exited bool) func() {
	if e.logDrain == nil {
		return func() {}
	}
	release := e.logDrain(ctx, uuid, exited)
	if release == nil {
		return func() {}
	}
	return release
}
