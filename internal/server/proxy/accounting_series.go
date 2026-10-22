package proxy

import "time"

// pruneRoutingSeriesLocked bounds the per-instance and retry label maps. A
// deployment can be recreated indefinitely, so retaining every historical
// label forever would make the request path grow without limit. Active series
// remain cumulative; inactive labels are discarded after a day or at the hard
// cardinality cap.
func (a *Accountant) pruneRoutingSeriesLocked(now time.Time) {
	prune := func(values map[string]uint64, seen map[string]time.Time) {
		for key, at := range seen {
			if now.Sub(at) > accountingSeriesRetention {
				delete(seen, key)
				delete(values, key)
			}
		}
		for len(values) > maxAccountingSeries {
			var oldest string
			var oldestAt time.Time
			for key := range values {
				at := seen[key]
				if oldest == "" || at.Before(oldestAt) {
					oldest, oldestAt = key, at
				}
			}
			if oldest == "" {
				break
			}
			delete(values, oldest)
			delete(seen, oldest)
		}
	}
	prune(a.routed, a.routedSeen)
	prune(a.retries, a.retriesSeen)
}
