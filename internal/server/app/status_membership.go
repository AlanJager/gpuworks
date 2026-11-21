//go:build linux && dqlite

package app

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// statusEnrolledKeys counts the member keys this member's transport policy
// names, allowed and revoked. It is how a cluster of two that used to be
// larger is told from one that is still being formed: the policy keeps the
// key of a member that left. Zero means the policy could not be read.
func statusEnrolledKeys(cfg Config) int {
	path, _, err := ClusterTransportPolicyPath(cfg)
	if err != nil {
		return 0
	}
	policy, err := identity.LoadTransportPolicy(path)
	if err != nil {
		return 0
	}
	return len(policy.AllowedKeys) + len(policy.RevokedKeys)
}

// membershipShortfall says why a cluster whose business answers is still not
// at full strength, or returns "" when it is, or when that cannot be told.
//
// Only the membership the leader reported counts; the cluster.yaml cache can
// trail a cluster that has since reached three voters. A cluster of three or
// more members with fewer than three voters is short. A cluster of two is
// short only when more keys were enrolled than it has members, so one member
// left; two members with two keys are an installation between its second and
// third member. A single member is not a cluster yet.
func (s clusterStatus) membershipShortfall() string {
	live := 0
	for _, member := range s.members {
		if member.source == "live raft" {
			live++
		}
	}
	if live < 2 || s.voters >= 3 {
		return ""
	}
	if live == 2 && s.enrolledKeys <= live {
		return ""
	}
	voters := "voters"
	if s.voters == 1 {
		voters = "voter"
	}
	return fmt.Sprintf("%d %s among %d members, not fault tolerant", s.voters, voters, live)
}
