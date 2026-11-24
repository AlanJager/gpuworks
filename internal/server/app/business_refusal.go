//go:build linux && dqlite

package app

import (
	"log/slog"
	"math/bits"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// businessRefusalHeader names, on a refused request, why this member did not
// serve it. The value is one of the fixed codes below and nothing else: the
// refusal is answered before authentication, so it carries no address, no
// identity and no text from the request.
const businessRefusalHeader = "X-Gpuworks-Refusal"

// Why a member refuses business. The gate's own codes for a member whose
// material proof does not admit it are the proof's reasons with the prefix
// "proof_" (proof_expired, proof_missing, proof_stopped, ...).
const (
	refusalNoGeneration       = "no_business_generation"    // no business generation is installed
	refusalGenerationChanged  = "generation_changed"        // the generation was withdrawn under the request
	refusalClusterUpgrade     = "cluster_upgrade"           // business is closed for a cluster upgrade
	refusalNoHandler          = "no_business_handler"       // the member has no business handler or no store
	refusalSessionsUnroutable = "sessions_not_routable"     // the member cannot accept session calls
	refusalTargetNotCurrent   = "target_not_current"        // the member's target version is not current
	refusalProofPrefix        = "proof_"                    // the material proof does not admit the member
	refusalNoKnownLeader      = "no_known_leader"           // no leader was observed in the last few seconds
	refusalNotTheLeader       = "not_the_leader"            // a forwarded request reached a member that does not lead
	refusalLeaderUnreachable  = "leader_unreachable"        // the request could not be forwarded to the leader
	refusalPeerUnidentified   = "forward_peer_unidentified" // a forwarded request without a member certificate
	refusalForwardNotReady    = "forward_not_ready"         // the member route is not set up on this member
	refusalPeerUndecided      = "forward_peer_undecided"    // no answer about the forwarding member's admission
	refusalPeerNotAdmitted    = "forward_peer_not_admitted" // the forwarding member is not admitted (403)
)

// businessRefusals counts the business requests this process refused, by
// reason, and reports them: one WARN per reason at its first, second, fourth,
// eighth refusal and so on, and one INFO when a request is served again after
// a run of refusals. A request that is served costs one atomic read.
type businessRefusals struct {
	log atomic.Pointer[slog.Logger]
	// open is set while a run of refusals has not been followed by a served
	// request.
	open atomic.Bool

	mu       sync.Mutex
	counts   map[string]uint64
	runStart time.Time
	runCount uint64
}

// memberBusinessRefusals is this process's record: a server process is one
// member.
var memberBusinessRefusals = &businessRefusals{}

func (b *businessRefusals) logger() *slog.Logger {
	if log := b.log.Load(); log != nil {
		return log
	}
	return slog.Default()
}

// record counts one refusal and returns the reason's running count.
func (b *businessRefusals) record(reason string) uint64 {
	b.mu.Lock()
	if b.counts == nil {
		b.counts = map[string]uint64{}
	}
	b.counts[reason]++
	n := b.counts[reason]
	if b.runCount == 0 {
		b.runStart = time.Now()
	}
	b.runCount++
	b.mu.Unlock()
	b.open.Store(true)
	if bits.OnesCount64(n) == 1 {
		b.logger().Warn("refused a business request", "reason", reason, "refused", n)
	}
	return n
}

// served ends a run of refusals, if one is open.
func (b *businessRefusals) served() {
	if !b.open.Load() || !b.open.CompareAndSwap(true, false) {
		return
	}
	b.mu.Lock()
	refused, lasted := b.runCount, time.Since(b.runStart)
	b.runCount = 0
	b.mu.Unlock()
	if refused == 0 {
		return
	}
	b.logger().Info("business served again after refusals", "refused", refused,
		"lasted_ms", float64(lasted)/float64(time.Millisecond))
}

// snapshot is the count per reason since the process started.
func (b *businessRefusals) snapshot() map[string]uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]uint64, len(b.counts))
	for reason, n := range b.counts {
		out[reason] = n
	}
	return out
}

// serving marks every request that reaches next as served.
func (b *businessRefusals) serving(next http.Handler) http.Handler {
	if next == nil {
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !memberProbe(r) {
			b.served()
		}
		next.ServeHTTP(w, r)
	})
}

// markRefusal names the reason on the response and counts it. A probe asks
// about the member and is not business: it gets the header and no count.
func markRefusal(w http.ResponseWriter, r *http.Request, reason string) {
	w.Header().Set(businessRefusalHeader, reason)
	if r != nil && memberProbe(r) {
		return
	}
	memberBusinessRefusals.record(reason)
}

// refuseMemberBusiness answers as a closed member does and says why.
func refuseMemberBusiness(w http.ResponseWriter, r *http.Request, reason string) {
	markRefusal(w, r, reason)
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "member business service is not ready", http.StatusServiceUnavailable)
}
