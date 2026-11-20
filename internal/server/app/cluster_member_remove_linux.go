//go:build linux && dqlite

package app

import (
	"context"
	"errors"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// ErrRemovalBreaksQuorum means the live voter set cannot lose this member and
// still have two voters. The call does not change membership.
var ErrRemovalBreaksQuorum = errors.New("removing this member would leave fewer than two voters")

// RemoveClusterMember drops a live member only when the removal leaves at
// least two voters. It does not open the application or create a cluster.
func RemoveClusterMember(ctx context.Context, db *cluster.Database, id uint64) error {
	if db == nil {
		return cluster.ErrUnavailable
	}
	members, err := db.Members(ctx)
	if err != nil {
		return err
	}
	voters := 0
	var role string
	found := false
	for _, member := range members {
		if member.Role == "voter" {
			voters++
		}
		if member.ID == id {
			found = true
			role = member.Role
		}
	}
	if !found {
		return errors.New("member is not in the live cluster")
	}
	if len(members) < 2 || (role == "voter" && voters-1 < 2) {
		return ErrRemovalBreaksQuorum
	}
	return db.Remove(ctx, id)
}

// ReplaceClusterMember removes oldID only after newID is already a voter, so
// the voter count stays at least two. It promotes a joined spare when the
// current voter set can accept that promotion. It does not open the application.
func ReplaceClusterMember(ctx context.Context, db *cluster.Database, oldID, newID uint64) error {
	if db == nil {
		return cluster.ErrUnavailable
	}
	if oldID == newID {
		return errors.New("replacement must be a different member")
	}
	members, err := db.Members(ctx)
	if err != nil {
		return err
	}
	oldRole, newRole := "", ""
	voters := 0
	for _, member := range members {
		if member.Role == "voter" {
			voters++
		}
		if member.ID == oldID {
			oldRole = member.Role
		}
		if member.ID == newID {
			newRole = member.Role
		}
	}
	if oldRole != "voter" || newRole == "" {
		return errors.New("replacement requires a live voter and its joined successor")
	}
	if newRole != "voter" {
		if voters < 2 {
			return ErrRemovalBreaksQuorum
		}
		if err := db.Assign(ctx, newID, "voter"); err != nil {
			return err
		}
		voters++
	}
	if voters-1 < 2 {
		return ErrRemovalBreaksQuorum
	}
	return db.Remove(ctx, oldID)
}
