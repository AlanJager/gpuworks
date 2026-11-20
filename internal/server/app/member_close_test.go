//go:build linux && dqlite

package app

import (
	"errors"
	"time"
)

// closeWithEligibilityTimeout closes an App the way a member's runtime does,
// with the given bound on each withdrawal step instead of the production one.
func (a *App) closeWithEligibilityTimeout(timeout time.Duration) error {
	return a.lifetime.close(func() error { return a.withdrawMember(timeout) }, func() error {
		err := a.release()
		if a.ha == nil && a.joinedRaft != nil {
			// A fixture without a runtime still owns its Raft session.
			err = errors.Join(err, a.joinedRaft.Close())
		}
		return err
	})
}
