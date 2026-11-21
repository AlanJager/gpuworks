//go:build linux && dqlite

package app

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
	"gpuworks.ai/gpuworks/internal/server/enrollment"
)

// statusQuorumProofRead is the live read from the database port. Tests
// replace it with one snapshot.
var statusQuorumProofRead = readStatusQuorumProof

// readStatusQuorumProof asks the database processes themselves for the
// leader's quorum proof, with this member's certificate and its persisted
// transport policy. It is what the enrollment route's facts are built from,
// and it answers wherever the database port does: a member that formed the
// cluster and has not restarted since serves no quorum route, and a follower's
// route needs the leader's. The proof names no enrollment origin, so peers'
// enrollment and material routes stay unobserved after this read.
func readStatusQuorumProof(ctx context.Context, cfg Config, s clusterStatus, timeout time.Duration) (enrollment.ClusterQuorumFacts, bool) {
	none := enrollment.ClusterQuorumFacts{}
	if s.clusterID == "not recorded" || s.pin == "" || len(s.cert) == 0 || len(s.key) == 0 {
		return none, false
	}
	ca, err := readStatusFile(filepath.Join(cfg.DataDir, enrollmentDirectory, "ca.pem"))
	if err != nil {
		return none, false
	}
	now := time.Now().UTC()
	trust, err := identity.NewTrust(ca, s.clusterID, s.pin, now)
	if err != nil {
		return none, false
	}
	policy, _, err := ClusterTransportPolicyPath(cfg)
	if err != nil {
		return none, false
	}
	_, config, err := trust.TLSConfigsFromPolicy(policy, s.cert, s.key, now)
	if err != nil {
		return none, false
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Only the leader answers; this member first, then the cached members.
	addresses := []string{s.address}
	for _, member := range s.members {
		if member.address != "" && member.address != s.address {
			addresses = append(addresses, member.address)
		}
	}
	for _, address := range addresses {
		if callCtx.Err() != nil {
			return none, false
		}
		proof, err := cluster.ReadQuorumProof(callCtx, address, func(ctx context.Context, target string) (net.Conn, error) {
			if target != address {
				return nil, errors.New("status refuses a changed proof endpoint")
			}
			return (&tls.Dialer{Config: config}).DialContext(ctx, "tcp", target)
		})
		if err != nil {
			continue
		}
		facts := enrollment.ClusterQuorumFacts{
			ClusterID: s.clusterID, CAFingerprint: s.pin, Protocol: s.protocol,
			Term: proof.Term, Index: proof.Index, ConfigurationIndex: proof.ConfigurationIndex, LeaderID: proof.LeaderID,
		}
		answeredByLeader := false
		for _, member := range proof.Members {
			facts.Members = append(facts.Members, enrollment.ClusterMember{ID: member.ID, Address: member.Address, Role: member.Role})
			if member.ID == proof.LeaderID && member.Address == address {
				answeredByLeader = true
			}
		}
		if answeredByLeader {
			return facts, true
		}
	}
	return none, false
}
