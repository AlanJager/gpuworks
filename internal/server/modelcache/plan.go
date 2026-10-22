// Package modelcache decides which node-cache model files to evict before an
// nfs_cache storage caches another model on a node. It is a pure function of
// the facts it is given: no store, no I/O. The scheduler's admission check and
// the controller's eviction use the same function, so a placement the
// evaluation admits is the placement the launch can make room for.
package modelcache

import (
	"sort"
	"time"
)

// Refusal codes.
const (
	RefuseCacheLimit       = "model_exceeds_cache_limit" // the model alone is larger than the cap
	RefuseInsufficientDisk = "insufficient_disk"         // evicting everything evictable leaves too little disk
	// RefuseCacheInUse: the disk has room, but the cap is taken by entries
	// that cannot be evicted (in use, still copying, or no longer backed).
	RefuseCacheInUse = "cache_full_in_use"
)

// Entry is one model already in the node cache.
type Entry struct {
	ID        uint
	SizeBytes int64
	// LastUsed is when an instance last started or stopped on it; nil sorts
	// as the oldest.
	LastUsed *time.Time
	// Referenced: an active deployment or a live instance on this node reads it.
	Referenced bool
	// Transferring: it is still being copied.
	Transferring bool
	// Backed: the storage still holds the authoritative copy.
	Backed bool
}

// Evictable reports whether the entry may be removed to make room.
func (e Entry) Evictable() bool { return !e.Referenced && !e.Transferring && e.Backed }

// Input is everything one decision needs.
type Input struct {
	// NeedBytes is the size of the model to cache.
	NeedBytes int64
	// FreeBytes is the free space on the cache disk now.
	FreeBytes int64
	// ReserveBytes is the free space the disk must keep.
	ReserveBytes int64
	// CapBytes caps the cache's total size on this node; 0 means no cap.
	CapBytes int64
	Entries  []Entry
}

// Result is the decision. Refusal is empty when the model fits.
type Result struct {
	Evict      []uint
	FreedBytes int64
	Refusal    string
	// NeedDiskBytes and NeedCapBytes are how much had to be freed for the disk
	// reserve and for the cap; they explain a refusal or an eviction.
	NeedDiskBytes int64
	NeedCapBytes  int64
}

// Plan evicts the least recently used evictable entries, oldest first, until
// both the disk reserve and the cache cap hold with the new model in place.
func Plan(in Input) Result { return plan(in, false) }

// Trim applies a reduced budget to existing cache. Unlike admission it keeps
// safe partial work when the remaining protected entries prevent convergence.
func Trim(in Input) Result { in.NeedBytes = 0; return plan(in, true) }

func plan(in Input, partial bool) Result {
	need := max64(in.NeedBytes, 0)
	if in.CapBytes > 0 && need > in.CapBytes {
		return Result{Refusal: RefuseCacheLimit, NeedCapBytes: need - in.CapBytes}
	}
	var total int64
	evictable := make([]Entry, 0, len(in.Entries))
	for _, entry := range in.Entries {
		total += max64(entry.SizeBytes, 0)
		if entry.Evictable() {
			evictable = append(evictable, entry)
		}
	}
	sort.SliceStable(evictable, func(i, j int) bool {
		a, b := evictable[i].LastUsed, evictable[j].LastUsed
		switch {
		case a == nil && b == nil:
			return evictable[i].ID < evictable[j].ID
		case a == nil:
			return true
		case b == nil:
			return false
		case a.Equal(*b):
			return evictable[i].ID < evictable[j].ID
		default:
			return a.Before(*b)
		}
	})
	out := Result{
		NeedDiskBytes: max64(need-(in.FreeBytes-in.ReserveBytes), 0),
	}
	if in.CapBytes > 0 {
		out.NeedCapBytes = max64(total+need-in.CapBytes, 0)
	}
	fits := func(freed int64) bool {
		if in.FreeBytes+freed-need < in.ReserveBytes {
			return false
		}
		return in.CapBytes <= 0 || total-freed+need <= in.CapBytes
	}
	for _, entry := range evictable {
		if fits(out.FreedBytes) {
			break
		}
		out.Evict = append(out.Evict, entry.ID)
		out.FreedBytes += max64(entry.SizeBytes, 0)
	}
	if !fits(out.FreedBytes) {
		refusal := RefuseInsufficientDisk
		if in.FreeBytes+out.FreedBytes-need >= in.ReserveBytes {
			refusal = RefuseCacheInUse
		}
		if partial {
			out.Refusal = refusal
			return out
		}
		return Result{Refusal: refusal, NeedDiskBytes: out.NeedDiskBytes, NeedCapBytes: out.NeedCapBytes}
	}
	return out
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
