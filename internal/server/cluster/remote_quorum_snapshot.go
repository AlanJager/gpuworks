package cluster

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// ReadRemoteExistingReadSnapshot brackets fixed SELECT/schema queries with
// fresh barriers from one remote leader. dial must authenticate and pin that
// member's exact public key on every connection. All borrowed SQL/socket
// cleanup completes before B or return; callers must retain any borrowed
// runtime lease until return. Success is an observation, never startup authority.
func ReadRemoteExistingReadSnapshot(parent context.Context, member Member, name string, dial func(context.Context, string) (net.Conn, error), read func(context.Context, *sql.Tx, QuorumProof) error) (QuorumProof, error) {
	return readRemoteExistingReadSnapshot(parent, member, name, dial, read, OpenRemoteExistingReadSQL)
}

func readRemoteExistingReadSnapshot(parent context.Context, member Member, name string, dial func(context.Context, string) (net.Conn, error), read func(context.Context, *sql.Tx, QuorumProof) error, open func(context.Context, Member, string, func(context.Context, string) (net.Conn, error)) (*sql.DB, func() error, error)) (proof QuorumProof, result error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	defer func() {
		result = errors.Join(result, ctx.Err())
		if result != nil {
			proof = QuorumProof{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return QuorumProof{}, err
	}
	address, err := identity.NormalizeMemberAddress(member.Address)
	if err != nil || address != member.Address || member.ID == 0 || member.Role != "voter" {
		return QuorumProof{}, errors.New("remote quorum snapshot requires an exact canonical voter identity")
	}
	if err := validateExistingReadName(name); err != nil {
		return QuorumProof{}, err
	}
	if dial == nil || read == nil || open == nil {
		return QuorumProof{}, errors.New("remote quorum snapshot requires a pinned dialer and fixed reader")
	}
	first, err := ReadQuorumProof(ctx, address, dial)
	if err != nil {
		return QuorumProof{}, err
	}
	if !remoteQuorumLeader(member, first) {
		return QuorumProof{}, errors.New("remote quorum snapshot leader differs from its pinned member")
	}
	if err := readQuorumSQL(ctx, first, func(ctx context.Context) (*sql.DB, func() error, error) {
		return open(ctx, member, name, dial)
	}, read); err != nil {
		return QuorumProof{}, err
	}
	if err := ctx.Err(); err != nil {
		return QuorumProof{}, err
	}
	last, err := ReadQuorumProof(ctx, address, dial)
	if err != nil {
		return QuorumProof{}, err
	}
	if !remoteQuorumLeader(member, last) || last.Index <= first.Index || !sameQuorumConfiguration(first, last) {
		return QuorumProof{}, errors.New("remote quorum configuration changed across SQL snapshot")
	}
	return last, nil
}

func remoteQuorumLeader(expected Member, proof QuorumProof) bool {
	if proof.LeaderID != expected.ID {
		return false
	}
	for _, member := range proof.Members {
		if member.ID == expected.ID {
			return member == expected
		}
	}
	return false
}
