package cluster

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// ObserveRaftLeader follows one direct leader hint with an independent fresh
// quorum proof. dial must verify the cluster authority, destination SAN and
// transport policy, returning the actual authenticated peer key. The returned
// tuple/key is only an observation to pin later reads, never admission or
// startup authority. Both physical connections finish cleanup before return.
func ObserveRaftLeader(parent context.Context, address string, dial func(context.Context, string) (net.Conn, string, error)) (member Member, key string, result error) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	defer func() {
		result = errors.Join(result, raftObservationContextError(ctx))
		if result != nil {
			member, key = Member{}, ""
		}
	}()
	if err := raftObservationContextError(ctx); err != nil {
		return Member{}, "", err
	}
	canonical, err := identity.NormalizeMemberAddress(address)
	if err != nil || canonical != address || dial == nil {
		return Member{}, "", errors.New("leader observation requires a canonical seed and authenticated dialer")
	}
	hint, err := readRaftLeaderHint(ctx, address, dial)
	if err != nil {
		return Member{}, "", err
	}
	if err := raftObservationContextError(ctx); err != nil {
		return Member{}, "", err
	}
	proof, err := ReadQuorumProof(ctx, hint.Address, func(ctx context.Context, target string) (net.Conn, error) {
		if target != hint.Address {
			return nil, errors.New("leader observation refuses a changed proof endpoint")
		}
		conn, peerKey, err := dial(ctx, target)
		if !validObservedPeerKey(peerKey) {
			err = errors.Join(err, errors.New("leader observation requires an authenticated lowercase SHA256 peer key"))
		}
		if err == nil {
			key = peerKey
		}
		// ReadQuorumProof closes and joins even a socket returned with an error.
		return conn, err
	})
	if err != nil {
		return Member{}, "", err
	}
	hint.Role = "voter"
	if !remoteQuorumLeader(hint, proof) {
		return Member{}, "", errors.New("fresh leader proof differs from the seed hint")
	}
	return hint, key, nil
}

const raftLeaderMaxBody = 1040 // One ID and a padded address of at most 1024 bytes.

func readRaftLeaderHint(ctx context.Context, address string, dial func(context.Context, string) (net.Conn, string, error)) (member Member, result error) {
	conn, peerKey, err := dial(ctx, address)
	if !validObservedPeerKey(peerKey) {
		err = errors.Join(err, errors.New("leader hint requires an authenticated lowercase SHA256 peer key"))
	}
	if err != nil || conn == nil {
		var closeErr error
		if conn != nil {
			closeErr = conn.Close()
		}
		if err == nil {
			err = errors.New("leader hint dial returned no connection")
		}
		return Member{}, errors.Join(err, raftObservationContextError(ctx), closeErr)
	}
	var once sync.Once
	var closeErr error
	closeConn := func() { once.Do(func() { closeErr = conn.Close() }) }
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(closed); closeConn() })
	defer func() {
		if !stop() {
			<-closed
		}
		closeConn()
		result = errors.Join(result, raftObservationContextError(ctx), closeErr)
		if result != nil {
			member = Member{}
		}
	}()
	if err := raftObservationContextError(ctx); err != nil {
		return Member{}, err
	}
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return Member{}, err
	}
	// Protocol v1, LEADER request 0/schema 0/extra 0, unused body word zero.
	request := []byte{1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	for offset := 0; offset < len(request); offset += 8 {
		n, err := conn.Write(request[offset : offset+8])
		if n != 8 {
			err = errors.Join(err, io.ErrShortWrite)
		}
		if err != nil {
			return Member{}, fmt.Errorf("write leader hint request: %w", err)
		}
	}
	var header [8]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return Member{}, fmt.Errorf("read leader hint header: %w", err)
	}
	words := binary.LittleEndian.Uint32(header[:4])
	if words < 2 || words > raftLeaderMaxBody/8 || header[5] != 0 || binary.LittleEndian.Uint16(header[6:]) != 0 || (header[4] != 0 && header[4] != 1) {
		return Member{}, errors.New("unsupported or oversized leader hint response")
	}
	body := make([]byte, int(words)*8)
	if _, err := io.ReadFull(conn, body); err != nil {
		return Member{}, fmt.Errorf("read leader hint body: %w", err)
	}
	if header[4] == 0 {
		return Member{}, decodeQuorumProofFailure(body)
	}
	offset := 0
	id, err := quorumProofWord(body, &offset)
	if err != nil {
		return Member{}, err
	}
	target, err := quorumProofString(body, &offset, 1024)
	if err != nil {
		return Member{}, err
	}
	canonical, err := identity.NormalizeMemberAddress(target)
	if id == 0 || err != nil || canonical != target || offset != len(body) {
		return Member{}, errors.New("leader hint has no canonical leader or contains trailing data")
	}
	return Member{ID: id, Address: target}, nil
}

func validObservedPeerKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for _, value := range key {
		if !(value >= '0' && value <= '9' || value >= 'a' && value <= 'f') {
			return false
		}
	}
	return true
}

func raftObservationContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Socket and context timers are independent. An installed socket deadline
	// can expire before the context publishes Err; preserve the shared deadline
	// cause and reject observations that finish cleanup after that deadline.
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= 0 {
		return context.DeadlineExceeded
	}
	return nil
}
