//go:build linux && dqlite

package store

import (
	"context"
	"errors"
	"time"
)

// Bind only from native construction: SQL admissions and cached voter counts
// cannot supply the actual local Raft identity or transport certificate key.
func (s *Store) bindInferenceMembership(expected ClusterIdentity, id uint64, address, key string) {
	s.inferenceMembership = func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if id == 0 || !validClusterDigest(key, 32) {
			return ErrClusterIdentityConflict
		}
		normalized, err := normalizeClusterMemberAddress(address)
		if err != nil {
			return err
		}
		revision, members, err := s.GrantClusterMembership(ctx, expected, key)
		if err != nil {
			return err
		}
		found := false
		for _, member := range members {
			if member.ID == id && member.Address == normalized && member.KeyFingerprint == key {
				found = true
				break
			}
		}
		if !found {
			return errors.New("local inference owner is not a live admitted member")
		}
		// Confirmation compares the complete currently activated policy with a
		// second confirmed authorization grant. It never applies pending policy.
		policy, err := s.ConfirmTransportAuthorization(ctx, key, key)
		if err != nil {
			return err
		}
		if policy.SourceRevision != revision {
			return errors.New("inference authorization changed during admission")
		}
		return ctx.Err()
	}
}
