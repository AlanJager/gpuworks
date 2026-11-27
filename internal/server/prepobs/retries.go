package prepobs

import (
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

const (
	// RetryGrace is how long a retry stays visible past the wait it announced:
	// the next attempt's first frame, or the failure, normally replaces it
	// well within that. An agent that went quiet stops showing a retry.
	RetryGrace = 30 * time.Second
	// RetryLimit bounds the retries observed at once.
	RetryLimit = 8192
	// RetryDropReportEvery bounds how often Record reports that the limit
	// dropped retries, so a full map costs one log line a minute, not one per
	// status frame.
	RetryDropReportEvery = time.Minute
)

type retryEntry struct {
	node       uint
	transferID string
	retry      proto.ModelFetchRetry
	until      time.Time
}

// Retries are the downloads waiting to ask their address again, keyed by
// model-file key: the agents' latest proto.ModelFetchRetry. Like Copies they
// are evidence only, never persisted; a frame without a retry ends it.
type Retries struct {
	mu        sync.Mutex
	entries   map[string]retryEntry
	byNode    map[uint]map[string]struct{}
	lastSweep time.Time
	// dropped counts the retries the limit kept out since start, and
	// lastReport when Record last asked the caller to report them.
	dropped    uint64
	lastReport time.Time
}

func NewRetries() *Retries {
	return &Retries{entries: map[string]retryEntry{}, byNode: map[uint]map[string]struct{}{}}
}

// Record keeps a reporting node's retry, or ends the key's retry when the
// frame carries none or is not a download in progress. A retry the full map
// keeps out is counted; report is then the running count, nonzero at most once
// per RetryDropReportEvery, for the caller to log (the download still shows,
// without its retry).
func (r *Retries) Record(node uint, status proto.ModelFileStatus, now time.Time) (report uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.entries[status.Key]; ok {
		delete(r.entries, status.Key)
		r.unindex(old.node, status.Key)
	}
	if status.Retry == nil || status.State != proto.MFDownloading {
		return 0
	}
	if _, exists := r.entries[status.Key]; !exists && len(r.entries) >= RetryLimit {
		// A full map sweeps expired entries at most once per RetryGrace, like
		// expiring.put: the check is per status frame, four per second per
		// download.
		if now.Sub(r.lastSweep) >= RetryGrace {
			r.lastSweep = now
			r.sweepLocked(now)
		}
		if len(r.entries) >= RetryLimit {
			r.dropped++
			if r.lastReport.IsZero() || now.Sub(r.lastReport) >= RetryDropReportEvery {
				r.lastReport = now
				return r.dropped
			}
			return 0
		}
	}
	wait := time.Duration(max(status.Retry.WaitSeconds, 0)) * time.Second
	r.entries[status.Key] = retryEntry{node: node, transferID: status.TransferID, retry: *status.Retry, until: now.Add(wait + RetryGrace)}
	if r.byNode[node] == nil {
		r.byNode[node] = map[string]struct{}{}
	}
	r.byNode[node][status.Key] = struct{}{}
	return 0
}

// Dropped is how many retries the limit kept out since start.
func (r *Retries) Dropped() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

// Observe is the retry of the intent a row now holds: the row must still be
// downloading under the same transfer ID, and the retry not past its grace.
func (r *Retries) Observe(key, state, transferID string, now time.Time) (proto.ModelFetchRetry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[key]
	if !ok || state != proto.MFDownloading || entry.transferID != transferID || !now.Before(entry.until) {
		return proto.ModelFetchRetry{}, false
	}
	return entry.retry, true
}

// ForgetNode drops every retry a node reported, in the critical section that
// replaces or removes its session (Copies.ForgetNode).
func (r *Retries) ForgetNode(node uint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.byNode[node] {
		delete(r.entries, key)
	}
	delete(r.byNode, node)
}

func (r *Retries) sweepLocked(now time.Time) {
	for key, entry := range r.entries {
		if !now.Before(entry.until) {
			delete(r.entries, key)
			r.unindex(entry.node, key)
		}
	}
}

func (r *Retries) unindex(node uint, key string) {
	delete(r.byNode[node], key)
	if len(r.byNode[node]) == 0 {
		delete(r.byNode, node)
	}
}
