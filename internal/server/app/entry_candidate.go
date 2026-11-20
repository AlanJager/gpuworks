//go:build linux && dqlite

package app

import (
	"context"
	"time"
)

// entryCheckConfig is one installed cluster-entry check. Failure and expiry
// remove the member from the candidates. Candidacy is membership in the set.
type entryCheckConfig struct {
	check func(context.Context) error
	limit time.Duration
}

// SetEntryCheck installs the cluster-entry check for this process. A nil
// check leaves a business-ready member in the candidate set. A returned
// error, or a check that does not finish within limit, removes the member.
// A non-positive limit removes the member too, because the check has no time
// to succeed.
func (a *App) SetEntryCheck(check func(context.Context) error, limit time.Duration) {
	if a == nil {
		return
	}
	a.entryCheckMu.Lock()
	defer a.entryCheckMu.Unlock()
	if check == nil {
		a.entryCheck = nil
		return
	}
	a.entryCheck = &entryCheckConfig{check: check, limit: limit}
}

// entryCandidate reports whether this process may hold the cluster entry.
// Business readiness is required. An installed check that fails or does not
// finish before its deadline removes the member. Scheduling leadership stays
// a separate answer and does not restore candidacy.
func (a *App) entryCandidate(ctx context.Context, businessReady bool) bool {
	if a == nil || !businessReady {
		return false
	}
	a.entryCheckMu.Lock()
	configured := a.entryCheck
	a.entryCheckMu.Unlock()
	if configured == nil || configured.check == nil {
		return true
	}
	if configured.limit <= 0 {
		return false
	}
	checkCtx, cancel := context.WithTimeout(ctx, configured.limit)
	defer cancel()
	done := make(chan error, 1)
	// The check has to return when checkCtx ends. The deadline observation
	// does not wait for a check that ignores cancellation.
	go func() {
		done <- configured.check(checkCtx)
	}()
	select {
	case err := <-done:
		return err == nil && checkCtx.Err() == nil
	case <-checkCtx.Done():
		return false
	}
}
