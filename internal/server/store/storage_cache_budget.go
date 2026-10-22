package store

import (
	"encoding/json"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/modelcache"
)

// CacheBudget uses a single loaded target set and the current directory's disk
// snapshot. A report about a previous directory never validates a new one.
func CacheBudget(storage *Storage, node *Node, targets []StorageNode, entries []modelcache.Entry, policy StoragePolicy, lastEvictedAt *time.Time) (modelcache.Input, bool, bool) {
	labels, _ := NodeLabels(node.Labels)
	eff, applies := effectiveStorage(*storage, targets, node.ID, labels)
	in := modelcache.Input{CapBytes: eff.CacheMaxBytes, Entries: entries}
	if node.PathUsageObservedAt == nil || lastEvictedAt != nil && !node.PathUsageObservedAt.After(*lastEvictedAt) {
		return in, false, applies
	}
	var paths []proto.PathUsage
	_ = json.Unmarshal([]byte(node.PathUsage), &paths)
	for _, p := range paths {
		if p.Kind != proto.PathKindStorageCache || p.StorageID != storage.ID || eff.CachePath == "" || filepath.Clean(p.Path) != filepath.Clean(eff.CachePath) || p.TotalBytes <= 0 {
			continue
		}
		in.FreeBytes = p.AvailableBytes
		in.ReserveBytes = policy.ReserveFreeBytes
		if in.ReserveBytes <= 0 {
			in.ReserveBytes = DefaultReserveFreeBytes(p.TotalBytes)
		}
		return in, true, applies
	}
	return in, false, applies
}

// CacheBudgetStatus is the read-side budget projection: the API needs whether
// cleanup can make progress and why protection prevents full convergence.
func CacheBudgetStatus(in modelcache.Input) (needsCleanup bool, reason string) {
	result := modelcache.Trim(in)
	return len(result.Evict) > 0, result.Refusal
}
