package app

import "context"

// services are the background loops Run starts with no ordering against
// anything else: the deployments' health checks, the cards' failure records,
// the daily usage report, the notices' refresh, then each extension module's
// Service. Each runs until ctx is done and Run waits for all of them.
func (a *App) services() []func(context.Context) {
	out := []func(context.Context){a.health.Run, a.cards.run, a.usage.Run, a.notices.Run}
	if a.ext != nil {
		out = append(out, a.ext.services...)
	}
	return out
}
