package cluster

import (
	"bytes"
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

// QuorumProof describes one barrier and its committed voter configuration.
// It is a transport response, not a cached or durable admission decision.
type QuorumProof struct {
	Term               uint64
	Index              uint64
	ConfigurationIndex uint64
	LeaderID           uint64
	Members            []Member
}

const quorumProofMaxBody = 64 * 1024

// ReadQuorumProof requests the repository's barrier-capable cluster wire format.
// The supplied dial must authenticate and pin the intended member transport.
// Old servers and ordinary cluster responses are refused without fallback.
func ReadQuorumProof(parent context.Context, address string, dial func(context.Context, string) (net.Conn, error)) (proof QuorumProof, result error) {
	if dial == nil {
		return proof, errors.New("quorum proof requires an authenticated dialer")
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return proof, err
	}
	conn, err := dial(ctx, address)
	if err != nil || conn == nil {
		var closeErr error
		if conn != nil {
			closeErr = conn.Close()
		}
		if err == nil {
			err = errors.New("quorum proof dial returned no connection")
		}
		return proof, errors.Join(err, ctx.Err(), closeErr)
	}

	// Cancellation owns the actual Close. A return waits for that callback,
	// including a blocked Close, before exposing its proof or error.
	var once sync.Once
	var closeErr error
	closeConn := func() { once.Do(func() { closeErr = conn.Close() }) }
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(closed)
		closeConn()
	})
	defer func() {
		if !stop() {
			<-closed
		}
		closeConn()
		result = errors.Join(result, ctx.Err(), closeErr)
		if result != nil {
			proof = QuorumProof{}
		}
	}()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return proof, err
	}

	// Literal protocol-v1 handshake, then CLUSTER format 2: one body word,
	// request type 16, schema/extra zero. No SQL or leader-discovery request.
	request := []byte{
		1, 0, 0, 0, 0, 0, 0, 0,
		1, 0, 0, 0, 16, 0, 0, 0,
		2, 0, 0, 0, 0, 0, 0, 0,
	}
	for offset := 0; offset < len(request); offset += 8 {
		n, err := conn.Write(request[offset : offset+8])
		if n != 8 {
			err = errors.Join(err, io.ErrShortWrite)
		}
		if err != nil {
			return proof, fmt.Errorf("write quorum proof request: %w", err)
		}
	}
	var header [8]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return proof, fmt.Errorf("read quorum proof header: %w", err)
	}
	words := binary.LittleEndian.Uint32(header[:4])
	if words == 0 || words > quorumProofMaxBody/8 {
		return proof, errors.New("quorum proof response body is outside its size bound")
	}
	responseType, schema := header[4], header[5]
	if binary.LittleEndian.Uint16(header[6:]) != 0 || !((responseType == 3 && schema == 2) || (responseType == 0 && schema == 0)) {
		return proof, fmt.Errorf("unsupported quorum proof response type %d schema %d extra %d", responseType, schema, binary.LittleEndian.Uint16(header[6:]))
	}
	body := make([]byte, int(words)*8)
	if _, err := io.ReadFull(conn, body); err != nil {
		return proof, fmt.Errorf("read quorum proof body: %w", err)
	}
	if responseType == 0 {
		return proof, decodeQuorumProofFailure(body)
	}
	proof, err = decodeQuorumProof(body)
	if err != nil {
		return QuorumProof{}, err
	}
	if err := ctx.Err(); err != nil {
		return QuorumProof{}, err
	}
	return proof, nil
}

func decodeQuorumProof(body []byte) (QuorumProof, error) {
	proof := QuorumProof{}
	offset := 0
	values := make([]uint64, 6)
	for i := range values {
		value, err := quorumProofWord(body, &offset)
		if err != nil {
			return proof, err
		}
		values[i] = value
	}
	if values[0] != 1 || values[1] == 0 || values[2] == 0 || values[3] == 0 || values[3] > values[2] || values[4] == 0 || values[5] == 0 || values[5] > 512 {
		return proof, errors.New("invalid quorum proof barrier or configuration")
	}
	proof.Term, proof.Index, proof.ConfigurationIndex, proof.LeaderID = values[1], values[2], values[3], values[4]
	proof.Members = make([]Member, 0, int(values[5]))
	ids := make(map[uint64]bool)
	addresses := make(map[string]bool)
	leaderVoter := false
	for i := uint64(0); i < values[5]; i++ {
		id, err := quorumProofWord(body, &offset)
		if err != nil {
			return QuorumProof{}, err
		}
		address, err := quorumProofString(body, &offset, 1024)
		if err != nil {
			return QuorumProof{}, err
		}
		role, err := quorumProofWord(body, &offset)
		if err != nil {
			return QuorumProof{}, err
		}
		canonical, canonicalErr := identity.NormalizeMemberAddress(address)
		if id == 0 || ids[id] || addresses[address] || canonicalErr != nil || canonical != address || role > 2 {
			return QuorumProof{}, errors.New("invalid or duplicate quorum proof member")
		}
		roleName := []string{"voter", "stand-by", "spare"}[role]
		ids[id], addresses[address] = true, true
		proof.Members = append(proof.Members, Member{ID: id, Address: address, Role: roleName})
		if id == proof.LeaderID && role == 0 {
			leaderVoter = true
		}
	}
	if offset != len(body) || !leaderVoter {
		return QuorumProof{}, errors.New("quorum proof has trailing data or no voter leader")
	}
	return proof, nil
}

func decodeQuorumProofFailure(body []byte) error {
	offset := 0
	code, err := quorumProofWord(body, &offset)
	if err != nil {
		return err
	}
	message, err := quorumProofString(body, &offset, quorumProofMaxBody-8)
	if err != nil {
		return err
	}
	if offset != len(body) {
		return errors.New("quorum proof failure has trailing data")
	}
	return fmt.Errorf("quorum proof server failure %d: %s", code, message)
}

func quorumProofWord(body []byte, offset *int) (uint64, error) {
	if len(body)-*offset < 8 {
		return 0, io.ErrUnexpectedEOF
	}
	word := binary.LittleEndian.Uint64(body[*offset : *offset+8])
	*offset += 8
	return word, nil
}

func quorumProofString(body []byte, offset *int, maximum int) (string, error) {
	remaining := body[*offset:]
	search := remaining
	if len(search) > maximum+1 {
		search = search[:maximum+1]
	}
	end := bytes.IndexByte(search, 0)
	if end < 0 {
		return "", errors.New("quorum proof string is unterminated or exceeds its bound")
	}
	padded := (end + 1 + 7) &^ 7
	if padded > len(remaining) {
		return "", io.ErrUnexpectedEOF
	}
	for _, value := range remaining[end:padded] {
		if value != 0 {
			return "", errors.New("quorum proof string has nonzero padding")
		}
	}
	*offset += padded
	return string(remaining[:end]), nil
}
