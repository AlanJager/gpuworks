//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// A member whose replication passes keep ending without completing is
// reported once it has lasted this long, and again at this rate while it
// lasts: one line that names the members not answering, instead of one per
// pass every two seconds.
const (
	materialPendingReportAfter = 30 * time.Second
	materialPendingReportEvery = 5 * time.Minute
)

// materialPendingNotice follows a run of replication passes that ended
// without completing and without closing the member.
type materialPendingNotice struct {
	mu       sync.Mutex
	since    time.Time
	reported time.Time
	passes   int
}

// pending records one such pass and says whether it is time to report.
func (n *materialPendingNotice) pending(now time.Time) (report bool, since time.Time, passes int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.since.IsZero() {
		n.since = now
	}
	n.passes++
	if now.Sub(n.since) < materialPendingReportAfter ||
		(!n.reported.IsZero() && now.Sub(n.reported) < materialPendingReportEvery) {
		return false, n.since, n.passes
	}
	n.reported = now
	return true, n.since, n.passes
}

// ended closes the run and says whether it had been reported.
func (n *materialPendingNotice) ended() (reported bool, since time.Time, passes int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	reported, since, passes = !n.reported.IsZero(), n.since, n.passes
	n.since, n.reported, n.passes = time.Time{}, time.Time{}, 0
	return reported, since, passes
}

// silentMaterialMember is one member a pass skipped as unreachable, and why,
// on one bounded line.
func silentMaterialMember(member store.ClusterTransportMember, err error) string {
	reason := strings.Join(strings.Fields(err.Error()), " ")
	if len(reason) > 160 {
		reason = reason[:160]
	}
	return fmt.Sprintf("member %d at %s: %s", member.ID, member.Address, reason)
}

// namingSilentMaterialMembers adds the skipped members to the error of a
// pass that found too few members verified, so that the report of a pending
// run says who is not answering. Any other error is returned as it is.
func namingSilentMaterialMembers(err error, silent []string) error {
	if !errors.Is(err, store.ErrMaterialManifestNoMajority) || len(silent) == 0 {
		return err
	}
	return fmt.Errorf("%w; not answering: %s", err, strings.Join(silent, "; "))
}

// materialReplicationPassFailed ends a replication pass that returned err
// while this member keeps running. Only a verdict closes the member; a pass
// that ran out of time, found no database leader or got too few answers
// stays pending, and the next pass repeats it. Meanwhile the member serves
// on its proof, a separate check with its own majority, which is at most
// materialEligibilityWindow old, or materialUnconfirmedLimit when the proof
// itself gets no answer.
func (a *App) materialReplicationPassFailed(err error) {
	closes := materialReplicationFailureInvalidates(err)
	if closes {
		a.invalidateCandidateMaterialEligibility(closeMaterialPass)
		a.materialPending.ended()
		if a.log != nil {
			a.log.Warn("cluster material replication pending", "err", err, "closes_member", true)
		}
		return
	}
	report, since, passes := a.materialPending.pending(time.Now())
	if a.log == nil {
		return
	}
	if report {
		a.log.Warn("cluster material replication has not completed; this member keeps serving on its last confirmation",
			"pending_for", time.Since(since).Round(time.Second).String(), "passes", passes, "err", err)
		return
	}
	a.log.Debug("cluster material replication pending", "err", err, "closes_member", false)
}

// materialReplicationPassCompleted ends a run of pending passes.
func (a *App) materialReplicationPassCompleted() {
	reported, since, passes := a.materialPending.ended()
	if reported && a.log != nil {
		a.log.Info("cluster material replication completed again",
			"was_pending_for", time.Since(since).Round(time.Second).String(), "passes", passes)
	}
}

func waitForMaterialReplicationCycle(ctx context.Context, tick <-chan time.Time, interval time.Duration, failed bool) bool {
	if ctx.Err() != nil {
		return false
	}
	if failed {
		// The next iteration observes current identity and membership and
		// performs a new complete cycle. This wait never repeats a grant.
		retry := time.NewTimer(min(interval, materialReproofInterval))
		defer retry.Stop()
		tick = retry.C
	}
	select {
	case <-ctx.Done():
		return false
	case <-tick:
		return ctx.Err() == nil
	}
}
