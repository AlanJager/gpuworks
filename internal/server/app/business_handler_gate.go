//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// businessHandlerGate keeps a closed member's HTTP handler stable while its
// business handler is installed or withdrawn. It starts closed; a later stage
// must enforce candidate admission before installation and per request.
type businessHandlerGate struct {
	closed              http.Handler
	mu                  sync.Mutex
	active              *businessHandler
	materialRequired    atomic.Bool
	materialEligibility atomic.Pointer[materialEligibility]
	readinessLog        *slog.Logger
	// clusterUpgrade is set while this member's business is closed for a
	// cluster upgrade; the closed answer then says so.
	clusterUpgrade atomic.Bool
	// upgradeProgress answers the public progress document of a cluster
	// upgrade in either state; nil for a gate without a database session.
	upgradeProgress *clusterUpgradeProgress
}

type businessHandler struct {
	http.Handler
	inFlight int
	retired  bool
	drained  chan struct{}
}

// businessHandlerDrain retains the old generation's completion signal across
// bounded waits, so a timed-out caller can finish cleanup after it drains.
type businessHandlerDrain struct {
	done <-chan struct{}
}

func newBusinessHandlerGate(loggers ...*slog.Logger) *businessHandlerGate {
	gate := &businessHandlerGate{}
	closed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if gate.clusterUpgrade.Load() && wantsUpgradePage(r) {
			serveClusterUpgradePage(w)
			return
		}
		if gate.clusterUpgrade.Load() {
			markRefusal(w, r, refusalClusterUpgrade)
			// The same status as any closed member, so an entry layer keeps
			// treating it as not ready; the code tells a client why, and
			// that the answer will change without its doing anything.
			w.Header().Set("Retry-After", "5")
			messages.WriteHTTPError(w, http.StatusServiceUnavailable, messages.NewProductError(api.ErrorClusterUpgradeInProgress,
				"the control-plane cluster is being upgraded; the request was not processed", nil))
			return
		}
		reason, _ := r.Context().Value(gateRefusalKey{}).(string)
		if reason == "" {
			reason = refusalNoGeneration
		}
		markRefusal(w, r, reason)
		http.Error(w, "member business service is not ready", http.StatusServiceUnavailable)
	})
	var log *slog.Logger
	if len(loggers) != 0 {
		log = loggers[0]
	}
	gate.closed, gate.readinessLog = closed, log
	return gate
}

type gateRefusalKey struct{}

// refuse answers as a closed member and names the reason: the gate's own
// reason, as its readiness log line has it, translated to its refusal code.
func (g *businessHandlerGate) refuse(w http.ResponseWriter, r *http.Request, reason string) {
	g.closed.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), gateRefusalKey{}, gateRefusalCode(reason))))
}

func (g *businessHandlerGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The public probe reports process liveness in either state.
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		g.closed.ServeHTTP(w, r)
		return
	}
	if g.upgradeProgress != nil && r.URL.Path == ClusterUpgradeProgressPath {
		g.upgradeProgress.serve(w, r, g.businessOpen())
		return
	}
	if g.materialRequired.Load() {
		ready, reason, _ := g.materialEligibility.Load().eligibleForResult(0)
		if !ready && r.URL.Path != "/api/v1/readyz" {
			// Probes report the current state immediately; business requests
			// may wait out a transient closure.
			g.awaitTransientReopen(r.Context(), businessTransientReopenWait)
			ready, reason, _ = g.materialEligibility.Load().eligibleForResult(0)
		}
		if !ready {
			g.refuse(w, r, reason)
			if r.Method == http.MethodGet && r.URL.Path == "/api/v1/readyz" {
				g.logReadinessRejection(r, "entry", reason, time.Time{}, 0, 0)
			}
			return
		}
	}
	g.mu.Lock()
	active := g.active
	if active != nil {
		active.inFlight++
	}
	g.mu.Unlock()
	if active != nil {
		defer g.leave(active)
		active.ServeHTTP(w, r)
		return
	}
	g.refuse(w, r, refusalNoGeneration)
	if r.Method == http.MethodGet && r.URL.Path == "/api/v1/readyz" {
		g.logReadinessRejection(r, "entry", "no_business_generation", time.Time{}, 0, 0)
	}
}

// businessOpen reports whether a request arriving now would reach the
// business handler.
func (g *businessHandlerGate) businessOpen() bool {
	if g.materialRequired.Load() {
		if ready, _, _ := g.materialEligibility.Load().eligibleForResult(0); !ready {
			return false
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active != nil
}

func (g *businessHandlerGate) requireMaterialEligibility() {
	g.materialRequired.Store(true)
}

func (g *businessHandlerGate) setMaterialEligibility(eligibility *materialEligibility) {
	g.materialEligibility.Store(eligibility)
}

// setBusinessHandler installs a handler for later requests; nil withdraws it.
// Requests that already entered a handler finish through that handler.
//
// A request is admitted on what this process already holds: the current
// generation and the member's last confirmed proof, which a background worker
// renews and which ends on a refusal or when it cannot be renewed inside its
// window. No request waits for a replicated write to be admitted, so a
// storage stall shorter than that window delays no request and refuses none.
func (g *businessHandlerGate) setBusinessHandler(handler http.Handler) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.retire(g.active)
	if handler == nil {
		g.active = nil
	} else {
		active := &businessHandler{drained: make(chan struct{})}
		business := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			readiness := r.Method == http.MethodGet && r.URL.Path == "/api/v1/readyz"
			var started time.Time
			if readiness {
				started = time.Now()
			}
			// A withdrawal or an invalidation can land between the gate's
			// entry check and this handler. Recheck both before business.
			g.mu.Lock()
			current := g.active == active && !active.retired
			g.mu.Unlock()
			materialReady, reason := true, "generation_changed"
			if current && g.materialRequired.Load() {
				materialReady, reason, _ = g.materialEligibility.Load().eligibleForResult(0)
			}
			if !current || !materialReady {
				g.refuse(w, r, reason)
				if readiness {
					g.logReadinessRejection(r, "post_admission", reason, started, 0, 0)
				}
				return
			}
			if readiness {
				// Readiness SQL can outlast this handler's admission. Buffer only
				// this fixed response; a successful read is published after a
				// final generation and proof check. SQL is never replayed, and
				// ordinary in-flight business keeps its existing lifecycle.
				eligibility := g.materialEligibility.Load()
				epoch := eligibility.readinessEpoch()
				response := &capturingResponse{}
				handler.ServeHTTP(response, r)
				currentReason := "renewal_unavailable"
				currentReady := func() bool {
					g.mu.Lock()
					current := g.active == active && !active.retired
					g.mu.Unlock()
					if !current {
						currentReason = "generation_changed"
						return false
					}
					if r.Context().Err() != nil {
						currentReason = "request_canceled"
						return false
					}
					if !g.materialRequired.Load() {
						return true
					}
					if g.materialEligibility.Load() != eligibility {
						currentReason = "proof_owner_changed"
						return false
					}
					if valid, reason := eligibility.readinessEpochMatchesResult(epoch); !valid {
						currentReason = reason
						return false
					}
					valid, reason, _ := g.materialEligibility.Load().eligibleForResult(0)
					if !valid {
						currentReason = reason
					}
					return valid
				}
				if !currentReady() {
					g.refuse(w, r, currentReason)
					g.logReadinessRejection(r, "post_sql", currentReason, started, 0, 0)
					return
				}
				publish := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					// A nearly expired proof must not advertise readiness: an entry
					// layer should stop choosing this member one membership-grant
					// timeout before its business closes. Ordinary business
					// retains its existing eligibility and lifecycle.
					if !currentReady() {
						g.refuse(w, r, currentReason)
						g.logReadinessRejection(r, "final_publish", currentReason, started, 0, 0)
						return
					}
					if g.materialRequired.Load() {
						valid, reason, remaining := g.materialEligibility.Load().eligibleForResult(store.ClusterMembershipGrantTimeout)
						if !valid {
							g.refuse(w, r, reason)
							g.logReadinessRejection(r, "final_publish", reason, started, remaining, store.ClusterMembershipGrantTimeout)
							return
						}
					}
					response.replay(w)
				})
				if response.status == http.StatusOK && g.materialRequired.Load() {
					waitHeadroom := store.ClusterMembershipGrantTimeout
					if eligibility != nil {
						waitHeadroom += eligibility.interval
					}
					if !eligibility.EligibleFor(waitHeadroom) {
						// A renewal already in flight may restore the required
						// headroom. Reserve its existing scheduling interval; the
						// final five-second check remains. This never starts a
						// proof or repeats the buffered SQL.
						if err := eligibility.waitForReadinessRenewal(r.Context(), store.ClusterMembershipGrantTimeout, waitHeadroom, epoch, currentReady); err != nil {
							g.refuse(w, r, currentReason)
							reason := currentReason
							if errors.Is(err, context.Canceled) {
								reason = "request_canceled"
							} else if errors.Is(err, context.DeadlineExceeded) {
								reason = "request_deadline"
							}
							g.logReadinessRejection(r, "renewal_wait", reason, started, 0, 0)
							return
						}
					}
				}
				publish.ServeHTTP(w, r)
				return
			}
			handler.ServeHTTP(w, r)
		})
		active.Handler = business
		g.active = active
	}
}

func (g *businessHandlerGate) logReadinessRejection(r *http.Request, stage, reason string, started time.Time, remaining, required time.Duration) {
	if g.readinessLog == nil {
		return
	}
	attrs := []any{"stage", stage, "reason", reason}
	if !started.IsZero() {
		attrs = append(attrs, "elapsed_ms", float64(min(max(time.Since(started), 0), time.Minute))/float64(time.Millisecond))
	}
	if required > 0 {
		attrs = append(attrs, "remaining_ms", float64(min(max(remaining, 0), materialEligibilityWindow))/float64(time.Millisecond), "required_ms", float64(required)/float64(time.Millisecond))
	}
	// No outer trusted request-ID source exists before the staged API pipeline.
	// Caller headers and low-level errors never cross this diagnostic boundary.
	g.readinessLog.DebugContext(r.Context(), "business readiness rejected", attrs...)
}

func (g *businessHandlerGate) materialReady() bool {
	eligibility := g.materialEligibility.Load()
	return eligibility != nil && eligibility.Eligible()
}

// withdrawBusinessHandler closes admission and waits only for the generation
// it withdrew. It returns the drain handle even on timeout, allowing a later
// bounded wait before releasing that generation's resources.
func (g *businessHandlerGate) withdrawBusinessHandler(ctx context.Context) (*businessHandlerDrain, error) {
	g.mu.Lock()
	old := g.active
	g.active = nil
	g.retire(old)
	g.mu.Unlock()
	if old == nil {
		return nil, nil
	}
	drain := &businessHandlerDrain{done: old.drained}
	return drain, drain.wait(ctx)
}

func (d *businessHandlerDrain) wait(ctx context.Context) error {
	select {
	case <-d.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retire is called with g.mu held.
func (g *businessHandlerGate) retire(handler *businessHandler) {
	if handler == nil || handler.retired {
		return
	}
	handler.retired = true
	if handler.inFlight == 0 {
		close(handler.drained)
	}
}

func (g *businessHandlerGate) leave(handler *businessHandler) {
	g.mu.Lock()
	handler.inFlight--
	if handler.retired && handler.inFlight == 0 {
		close(handler.drained)
	}
	g.mu.Unlock()
}
