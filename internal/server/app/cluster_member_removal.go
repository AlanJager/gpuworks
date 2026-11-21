package app

import (
	"errors"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// ErrRemovalBreaksQuorum means the voters that would remain and answer now
// could not commit a write, or would be a single voter. The call does not
// change membership.
var ErrRemovalBreaksQuorum = errors.New("removing this member would leave fewer than two voters that answer, or voters without a majority that answers")

// ErrRemovalLeavesTooFewMembers means fewer than three members would remain.
// A cluster of two cannot lose either member, and the database process keeps
// a single voter in a cluster under three. The call does not change membership.
var ErrRemovalLeavesTooFewMembers = errors.New("removing this member would leave fewer than three members")

// memberRemovalRefusal decides whether member id may leave the membership the
// leader reports. online says which members answer now; a member it does not
// confirm counts as offline, and without it nothing is removed.
//
// The voters that remain must have a majority that answers, whatever the
// caller asks: a removal that leaves the cluster without a leader cannot be
// undone through the cluster. Unless forced, two voters that answer and three
// members must remain. Force gives up only those two, for a cluster that has
// already lost a member for good.
func memberRemovalRefusal(members []cluster.Member, online func(cluster.Member) bool, id uint64, force bool) error {
	if online == nil {
		return errors.New("member removal requires an observation of which members answer")
	}
	found := false
	remaining, voters, answering := 0, 0, 0
	for _, member := range members {
		if member.ID == id {
			found = true
			continue
		}
		remaining++
		if member.Role != "voter" {
			continue
		}
		voters++
		if online(member) {
			answering++
		}
	}
	if !found {
		return errors.New("member is not in the live cluster")
	}
	if answering*2 <= voters || (!force && answering < 2) {
		return ErrRemovalBreaksQuorum
	}
	if !force && remaining < 3 {
		return ErrRemovalLeavesTooFewMembers
	}
	return nil
}

// memberReplacementRefusal decides whether oldID may be replaced by newID,
// and whether newID has to be promoted first. It judges the membership as it
// will be once newID votes, so a refusal comes before any role changes.
func memberReplacementRefusal(members []cluster.Member, online func(cluster.Member) bool, oldID, newID uint64, force bool) (promote bool, err error) {
	if oldID == newID {
		return false, errors.New("replacement must be a different member")
	}
	oldRole, newRole := "", ""
	voters := 0
	after := make([]cluster.Member, len(members))
	copy(after, members)
	for i, member := range after {
		if member.Role == "voter" {
			voters++
		}
		switch member.ID {
		case oldID:
			oldRole = member.Role
		case newID:
			newRole = member.Role
			after[i].Role = "voter"
		}
	}
	if oldRole != "voter" || newRole == "" {
		return false, errors.New("replacement requires a live voter and its joined successor")
	}
	promote = newRole != "voter"
	if promote && voters < 2 {
		return false, ErrRemovalBreaksQuorum
	}
	if err := memberRemovalRefusal(after, online, oldID, force); err != nil {
		return false, err
	}
	return promote, nil
}
