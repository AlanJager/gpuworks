//go:build linux && dqlite

package app

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// statusJoinGrace is how long after its transport policy last changed a
// cluster is still read as being formed. An admission changes the policy; the
// admitted member reaches the membership, and then its vote, some seconds
// later, and an installation goes on to restart each joined member under its
// service. Nothing this report can read tells an admitted member that has not
// arrived yet from one that left and kept its admission, so the age of the
// policy decides. Installations observed on fast disks took six to eight
// seconds from the last admission to membership and up to 105 seconds to the
// last restart; the bound is kept well above that for slow storage.
const statusJoinGrace = 5 * time.Minute

// observeTransportPolicy reads what this member's transport policy says about
// the cluster's admissions: how many member keys it names, how many of them
// are revoked, and when it last changed. The policy keeps the key of a member
// that left. An unreadable policy leaves all three zero.
func (s *clusterStatus) observeTransportPolicy(cfg Config) {
	path, _, err := ClusterTransportPolicyPath(cfg)
	if err != nil {
		return
	}
	policy, err := identity.LoadTransportPolicy(path)
	if err != nil {
		return
	}
	info, err := os.Lstat(path)
	if err != nil {
		return
	}
	s.enrolledKeys = len(policy.AllowedKeys) + len(policy.RevokedKeys)
	s.revokedKeys = len(policy.RevokedKeys)
	s.policyChanged = info.ModTime()
}

// liveMembers counts the members the leader reported. The cluster.yaml cache
// can trail a cluster that has since changed, so it is never counted.
func (s clusterStatus) liveMembers() int {
	live := 0
	for _, member := range s.members {
		if member.source == "live raft" {
			live++
		}
	}
	return live
}

// forming reports a cluster whose newest admission may still be on its way
// into the membership, or whose installation may still be starting its
// members: no member key was ever revoked, and the policy changed within
// statusJoinGrace of now. Once a key is revoked a member has left, and what
// the cluster lacks is no longer an installation in progress.
func (s clusterStatus) forming(now time.Time) bool {
	return s.enrolledKeys > 0 && s.revokedKeys == 0 && !s.policyChanged.IsZero() && now.Sub(s.policyChanged) < statusJoinGrace
}

// voterAnswers classifies the other voters the leader reported by what their
// database port did when this member connected to it. unreachable are the
// ones that said no: the connection was refused, or the network reported the
// host as unreachable. silent are the ones that gave no answer within the
// command's timeout. That is what a host that is down looks like, since the
// kernel keeps retrying such a connection, but it is weaker evidence: the
// packets may have been lost. The kernel of a running member completes the
// connection whatever its disk is doing, so a storage stall puts a voter in
// neither group; a member whose processes are frozen still answers.
func (s clusterStatus) voterAnswers() (unreachable, silent int) {
	database := make(map[uint64]string, len(s.peers))
	for _, peer := range s.peers {
		database[peer.id] = peer.database
	}
	for _, member := range s.members {
		if member.source != "live raft" || member.role != "voter" || member.id == s.id {
			continue
		}
		switch database[member.id] {
		case "connection refused", "unreachable":
			unreachable++
		case "timed out":
			silent++
		}
	}
	return unreachable, silent
}

// toleratesLosing reports that the voters left after missing of them are gone
// would still hold a majority once one more stopped.
func (s clusterStatus) toleratesLosing(missing int) bool {
	return (s.voters-missing-1)*2 > s.voters
}

// faultTolerance words the "Fault tolerant" line. A voter that is
// unreachable cannot be the one the cluster loses next; one that only gave
// no answer in time leaves the question open.
func (s clusterStatus) faultTolerance() string {
	unreachable, silent := s.voterAnswers()
	if !s.faultTolerant || !s.toleratesLosing(unreachable) {
		return "false"
	}
	if !s.toleratesLosing(unreachable + silent) {
		return "unverified (" + s.silentVoters(silent) + ")"
	}
	return "true"
}

func (s clusterStatus) silentVoters(silent int) string {
	return fmt.Sprintf("%d of %d voters did not answer within %s", silent, s.voters, s.probeTimeout)
}

// membershipShortfall says why a cluster whose business answers is still not
// at full strength, or returns "" when it is, or when that cannot be told.
//
// Only the membership the leader reported counts. A single member is not a
// cluster yet, and a cluster that is still being formed is not short of
// anything (see forming). Otherwise fewer than three voters is short, except
// in a cluster of two with no more keys enrolled than it has members: an
// installation between its second and third member. With three voters or
// more it is short when one of them is unreachable or gave no answer in
// time; the reason says which (see voterAnswers).
func (s clusterStatus) membershipShortfall(now time.Time) string {
	live := s.liveMembers()
	if live < 2 || s.forming(now) {
		return ""
	}
	if s.voters < 3 {
		if live == 2 && s.enrolledKeys <= live {
			return ""
		}
		voters := "voters"
		if s.voters == 1 {
			voters = "voter"
		}
		return fmt.Sprintf("%d %s among %d members, not fault tolerant", s.voters, voters, live)
	}
	unreachable, silent := s.voterAnswers()
	var reasons []string
	if unreachable > 0 {
		reasons = append(reasons, fmt.Sprintf("%d of %d voters unreachable", unreachable, s.voters))
	}
	if silent > 0 {
		reasons = append(reasons, s.silentVoters(silent))
	}
	if len(reasons) == 0 {
		return ""
	}
	switch {
	case !s.toleratesLosing(unreachable):
		reasons = append(reasons, "not fault tolerant")
	case !s.toleratesLosing(unreachable + silent):
		reasons = append(reasons, "fault tolerance unverified")
	}
	return strings.Join(reasons, ", ")
}
