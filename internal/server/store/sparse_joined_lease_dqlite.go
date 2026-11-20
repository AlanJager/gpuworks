//go:build linux && dqlite

package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// WithSparseJoinedLease borrows the original lease-only session for a bounded
// observation. It grants no membership, native startup or ownership transfer.
// use must finish its actual network cleanup before returning, and must not
// reenter locking session methods, including Close and AwaitingMembershipProof.
// Close waits for use to return; it does not cancel use or revoke its admission.
func (s *LegacyJoinedRaftSession) WithSparseJoinedLease(parent context.Context, use func(context.Context, string, uint64, string) error) (result error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || use == nil {
		return errors.New("sparse joined lease requires a session and reader")
	}
	if !s.mu.TryLock() {
		return errors.Join(ctx.Err(), errors.New("sparse joined session is busy"))
	}
	defer s.mu.Unlock()
	if s.db != nil || s.release == nil || s.id == 0 || s.owned {
		return errors.New("sparse joined lease is unavailable")
	}
	path, suffix := strings.CutSuffix(s.raftDir, ".dqlite")
	address, err := identity.NormalizeMemberAddress(s.address)
	if !suffix || !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) == "." || path == string(filepath.Separator) || err != nil || address != s.address {
		return errors.Join(ErrClusterIdentityConflict, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	result = use(ctx, path, s.id, s.address)
	result = errors.Join(result, ctx.Err())
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= 0 {
		result = errors.Join(result, context.DeadlineExceeded)
	}
	return result
}
