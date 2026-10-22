package proxy

import "time"

const (
	routeRefreshInterval = 2 * time.Second
	// routeEventRebuildGap is the least time between two rebuilds events
	// start: a fleet redeploying fires instance events by the thousand, and
	// rebuilding on each read every deployment and instance nonstop. A
	// request in the gap that misses the table falls back to its own
	// name-scoped store lookup.
	routeEventRebuildGap = 500 * time.Millisecond
	engineGaugeMaxAge    = 30 * time.Second
	kvSaturatedFraction  = 0.95
	kvSaturatedPenalty   = 8
	retryMaxAttempts     = 3
	retryWindow          = 10 * time.Second
	cooldownBase         = 5 * time.Second
	cooldownMax          = 60 * time.Second
	bodyMemoryThreshold  = 1 << 20
	bodyMemoryBudget     = 256 << 20
)
