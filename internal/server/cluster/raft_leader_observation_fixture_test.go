package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const observedSeedAddress = "seed.example:9002"

// Literal independent NODE and quorum frames over actual pipes. No native
// voter or TLS authentication is claimed by this ordinary transport fixture.
var observedLeaderHint = []byte{
	4, 0, 0, 0, 1, 0, 0, 0,
	11, 0, 0, 0, 0, 0, 0, 0,
	'n', 'o', 'd', 'e', '.', 'e', 'x', 'a', 'm', 'p', 'l', 'e', ':', '9', '0', '0', '1', 0, 0, 0, 0, 0, 0, 0,
}

var observedHintRequest = []byte{1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
var observedProofRequest = []byte{1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 16, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0}

type observedOriginal struct {
	net.Conn
	closed atomic.Bool
	calls  atomic.Int32
}

func (c *observedOriginal) Close() error {
	c.calls.Add(1)
	err := c.Conn.Close()
	c.closed.Store(true)
	return err
}

type observedLeaderRound struct {
	original *observedOriginal
	peer     net.Conn
	done     chan struct{}
	read     chan struct{}
	err      error
}

type observedLeaderFixture struct {
	packets          [][]byte
	keys             []string
	calls            atomic.Int32
	mu               sync.Mutex
	rounds           []*observedLeaderRound
	wrap             func(int, net.Conn) net.Conn
	dialErr          func(int) error
	stall            int
	candidateAddress string
}

func newObservedLeaderFixture(t *testing.T, hint []byte) *observedLeaderFixture {
	t.Helper()
	f := &observedLeaderFixture{packets: [][]byte{hint, quorumOwnerSuccess}, keys: []string{strings.Repeat("a", 64), strings.Repeat("b", 64)}, candidateAddress: "node.example:9001"}
	t.Cleanup(func() {
		f.mu.Lock()
		rounds := append([]*observedLeaderRound(nil), f.rounds...)
		f.mu.Unlock()
		for _, round := range rounds {
			_ = round.original.Conn.Close()
			_ = round.peer.Close()
			waitQuorumOwnerDone(t, round.done)
		}
	})
	return f
}

func (f *observedLeaderFixture) dial(_ context.Context, address string) (net.Conn, string, error) {
	call := int(f.calls.Add(1))
	expected := observedSeedAddress
	if call == 2 {
		expected = f.candidateAddress
		f.mu.Lock()
		closed := len(f.rounds) == 1 && f.rounds[0].original.closed.Load()
		f.mu.Unlock()
		if !closed {
			return nil, "", errors.New("candidate dial preceded seed original Close")
		}
	}
	if call > 2 || address != expected {
		return nil, "", fmt.Errorf("unexpected actual dial %d at %s", call, address)
	}
	client, peer := net.Pipe()
	original := &observedOriginal{Conn: client}
	round := &observedLeaderRound{original: original, peer: peer, done: make(chan struct{}), read: make(chan struct{})}
	f.mu.Lock()
	f.rounds = append(f.rounds, round)
	f.mu.Unlock()
	packet := append([]byte(nil), f.packets[call-1]...)
	go func() {
		defer close(round.done)
		defer peer.Close()
		request := make([]byte, 24)
		_, round.err = io.ReadFull(peer, request)
		close(round.read)
		want := observedHintRequest
		if call == 2 {
			want = observedProofRequest
		}
		if round.err == nil && !bytes.Equal(request, want) {
			round.err = errors.New("actual request differed from literal handshake/wire frame")
		}
		if round.err != nil {
			return
		}
		if f.stall == call {
			_, round.err = peer.Read(make([]byte, 1))
			if errors.Is(round.err, io.EOF) {
				round.err = nil
			}
			return
		}
		// Fragment responses independently of the caller's read boundaries.
		for _, value := range packet {
			if _, err := peer.Write([]byte{value}); err != nil {
				// Invalid-frame/key tests can intentionally close early.
				return
			}
		}
	}()
	var connection net.Conn = original
	if f.wrap != nil {
		connection = f.wrap(call, connection)
	}
	var err error
	if f.dialErr != nil {
		err = f.dialErr(call)
	}
	return connection, f.keys[call-1], err
}

func (f *observedLeaderFixture) check(t *testing.T, count int, cleanWire bool) {
	t.Helper()
	if f.calls.Load() != int32(count) {
		t.Fatalf("physical dials=%d want %d", f.calls.Load(), count)
	}
	f.mu.Lock()
	rounds := append([]*observedLeaderRound(nil), f.rounds...)
	f.mu.Unlock()
	if len(rounds) != count {
		t.Fatalf("original sockets=%d want %d", len(rounds), count)
	}
	for _, round := range rounds {
		waitQuorumOwnerDone(t, round.done)
		if !round.original.closed.Load() || round.original.calls.Load() != 1 {
			t.Fatal("original Close was not joined exactly once")
		}
		if cleanWire && round.err != nil {
			t.Fatal("literal peer request failed", round.err)
		}
	}
}

type observedLeaderResult struct {
	member Member
	key    string
	err    error
}

func assertEmptyLeaderObservation(t *testing.T, got observedLeaderResult, want error) {
	t.Helper()
	if got.err == nil || got.member != (Member{}) || got.key != "" || want != nil && !errors.Is(got.err, want) {
		t.Fatalf("invalid leader observation escaped or lost cause: %+v", got)
	}
}
