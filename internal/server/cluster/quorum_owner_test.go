package cluster

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Literal quorum packet whose leader address equals the discovery/dial tuple.
// This tests the local owner and parser; no remote native majority is asserted.
var quorumOwnerSuccess = []byte{
	11, 0, 0, 0, 3, 2, 0, 0,
	1, 0, 0, 0, 0, 0, 0, 0,
	7, 0, 0, 0, 0, 0, 0, 0,
	21, 0, 0, 0, 0, 0, 0, 0,
	20, 0, 0, 0, 0, 0, 0, 0,
	11, 0, 0, 0, 0, 0, 0, 0,
	1, 0, 0, 0, 0, 0, 0, 0,
	11, 0, 0, 0, 0, 0, 0, 0,
	'n', 'o', 'd', 'e', '.', 'e', 'x', 'a', 'm', 'p', 'l', 'e', ':', '9', '0', '0', '1', 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0,
}

type quorumOwnerResult struct {
	proof QuorumProof
	err   error
}

func waitQuorumOwnerDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("local quorum owner goroutine did not finish")
	}
}

func quorumOwnerStillWaiting(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatal("quorum owner returned before actual connection Close acknowledgement")
	case <-time.After(40 * time.Millisecond):
	}
}

func quorumOwnerStalledPeer(t *testing.T, peer net.Conn) (<-chan struct{}, <-chan error, <-chan struct{}, <-chan error, <-chan struct{}) {
	t.Helper()
	readDone, closedDone, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	readError, closeError := make(chan error, 1), make(chan error, 1)
	go func() {
		defer close(finished)
		defer peer.Close()
		request := make([]byte, 24)
		_, err := io.ReadFull(peer, request)
		want := []byte{1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 16, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0}
		if err == nil && !bytes.Equal(request, want) {
			err = errors.New("owner did not emit exact handshake and format2 request")
		}
		readError <- err
		close(readDone)
		if err != nil {
			closeError <- err
			close(closedDone)
			return
		}
		_, err = peer.Read(make([]byte, 1))
		closeError <- err
		close(closedDone)
	}()
	return readDone, readError, closedDone, closeError, finished
}

// Cache fixture completion so both assertions and failure teardown join it.
func quorumOwnerCachedFixture(t *testing.T, packet []byte) (net.Conn, <-chan struct{}, func() error) {
	t.Helper()
	dial, peer := quorumFixtureDial(t, packet, false)
	conn, err := dial(t.Context(), "node.example:9001")
	if err != nil {
		t.Fatal(err)
	}
	completed := make(chan struct{})
	var peerErr error
	go func() { peerErr = <-peer; close(completed) }()
	t.Cleanup(func() { _ = conn.Close(); waitQuorumOwnerDone(t, completed) })
	return conn, completed, func() error { return peerErr }
}

func TestDatabaseQuorumOwnerCloseCancelsAdmissionAndJoinsActualClose(t *testing.T) {
	client, peer := net.Pipe()
	closeFailure := errors.New("old quorum connection close acknowledgement failed")
	conn := &quorumHeldCloseConn{Conn: client, entered: make(chan struct{}), release: make(chan struct{}), failure: closeFailure}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(conn.release) }) }
	var leaderCalls, dialCalls atomic.Int32
	db := &Database{leader: func(context.Context) (Member, error) {
		leaderCalls.Add(1)
		return Member{ID: 11, Address: "node.example:9001", Role: "voter"}, nil
	}}
	bindDatabaseQuorumReader(db, func(_ context.Context, address string) (net.Conn, error) {
		dialCalls.Add(1)
		if address != "node.example:9001" {
			return nil, errors.New("unexpected owner dial address")
		}
		return conn, nil
	})
	readDone, readError, closedPeer, closeError, peerDone := quorumOwnerStalledPeer(t, peer)
	ctx, cancel := context.WithCancel(t.Context())
	oldResult := make(chan quorumOwnerResult, 1)
	oldDone, closeDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(oldDone)
		proof, err := db.QuorumProof(ctx)
		oldResult <- quorumOwnerResult{proof, err}
	}()
	closeStarted := false
	t.Cleanup(func() {
		release()
		cancel()
		_ = client.Close()
		_ = peer.Close()
		if closeStarted {
			waitQuorumOwnerDone(t, closeDone)
		} else {
			db.closeQuorum()
		}
		waitQuorumOwnerDone(t, oldDone)
		waitQuorumOwnerDone(t, peerDone)
	})
	waitQuorumOwnerDone(t, readDone)
	if err := <-readError; err != nil {
		t.Fatal(err)
	}
	closeStarted = true
	go func() { defer close(closeDone); db.closeQuorum() }()
	waitQuorumOwnerDone(t, conn.entered)
	waitQuorumOwnerDone(t, closedPeer)
	if err := <-closeError; !errors.Is(err, io.EOF) {
		t.Fatal("owner cancellation did not close actual peer stream", err)
	}
	proof, err := db.QuorumProof(t.Context())
	if err == nil || !reflect.DeepEqual(proof, QuorumProof{}) || leaderCalls.Load() != 1 || dialCalls.Load() != 1 {
		t.Fatalf("closed owner admitted another call: %#v/%v leader=%d dial=%d", proof, err, leaderCalls.Load(), dialCalls.Load())
	}
	quorumOwnerStillWaiting(t, oldDone)
	quorumOwnerStillWaiting(t, closeDone)
	release()
	waitQuorumOwnerDone(t, oldDone)
	waitQuorumOwnerDone(t, closeDone)
	old := <-oldResult
	if !errors.Is(old.err, context.Canceled) || !errors.Is(old.err, closeFailure) || !reflect.DeepEqual(old.proof, QuorumProof{}) {
		t.Fatalf("old call lost cancel/actualclose causes or returned proof: %#v", old)
	}
	db.closeQuorum()
	if conn.calls.Load() != 1 {
		t.Fatal("actual connection close repeated", conn.calls.Load())
	}
}

func TestDatabaseQuorumOwnerParentCancelKeepsNextCallAvailable(t *testing.T) {
	client, peer := net.Pipe()
	conn := &quorumHeldCloseConn{Conn: client, entered: make(chan struct{}), release: make(chan struct{})}
	close(conn.release)
	readDone, readError, closedPeer, closeError, peerDone := quorumOwnerStalledPeer(t, peer)
	healthyConn, healthyDone, healthyError := quorumOwnerCachedFixture(t, quorumOwnerSuccess)
	var leaderCalls, dialCalls atomic.Int32
	db := &Database{leader: func(context.Context) (Member, error) {
		leaderCalls.Add(1)
		return Member{ID: 11, Address: "node.example:9001", Role: "voter"}, nil
	}}
	bindDatabaseQuorumReader(db, func(ctx context.Context, address string) (net.Conn, error) {
		if dialCalls.Add(1) == 1 {
			return conn, nil
		}
		if address != "node.example:9001" {
			return nil, errors.New("unexpected healthy owner dial address")
		}
		return healthyConn, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	oldResult := make(chan quorumOwnerResult, 1)
	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		proof, err := db.QuorumProof(ctx)
		oldResult <- quorumOwnerResult{proof, err}
	}()
	t.Cleanup(func() {
		cancel()
		_ = client.Close()
		_ = peer.Close()
		db.closeQuorum()
		_ = healthyConn.Close()
		waitQuorumOwnerDone(t, healthyDone)
		waitQuorumOwnerDone(t, oldDone)
		waitQuorumOwnerDone(t, peerDone)
	})
	waitQuorumOwnerDone(t, readDone)
	if err := <-readError; err != nil {
		t.Fatal(err)
	}
	cancel()
	waitQuorumOwnerDone(t, closedPeer)
	if err := <-closeError; !errors.Is(err, io.EOF) {
		t.Fatal("parent cancellation left actual connection open", err)
	}
	waitQuorumOwnerDone(t, oldDone)
	old := <-oldResult
	if !errors.Is(old.err, context.Canceled) || !reflect.DeepEqual(old.proof, QuorumProof{}) || conn.calls.Load() != 1 {
		t.Fatalf("cancelled call returned %#v, close=%d", old, conn.calls.Load())
	}
	proof, err := db.QuorumProof(t.Context())
	if err != nil || proof.LeaderID != 11 || proof.Index != 21 || len(proof.Members) != 1 || proof.Members[0].Address != "node.example:9001" {
		t.Fatalf("parent cancel poisoned owner next actual connection: %#v/%v", proof, err)
	}
	waitQuorumOwnerDone(t, healthyDone)
	if err := healthyError(); err != nil {
		t.Fatal("healthy peer did not consume actual format2 request", err)
	}
	if leaderCalls.Load() != 2 || dialCalls.Load() != 2 {
		t.Fatalf("new independent call count leader=%d dial=%d", leaderCalls.Load(), dialCalls.Load())
	}
}

func TestDatabaseQuorumOwnerRejectsChangedDiscoveredLeader(t *testing.T) {
	for _, which := range []string{"id", "address"} {
		t.Run(which, func(t *testing.T) {
			leaderID := uint64(11)
			packet := quorumOwnerSuccess
			if which == "id" {
				leaderID = 12
			} else {
				packet = quorumFixtureSuccess
			}
			conn, peerDone, peerError := quorumOwnerCachedFixture(t, packet)
			db := &Database{leader: func(context.Context) (Member, error) {
				return Member{ID: leaderID, Address: "node.example:9001", Role: "voter"}, nil
			}}
			bindDatabaseQuorumReader(db, func(_ context.Context, address string) (net.Conn, error) {
				if address != "node.example:9001" {
					return nil, errors.New("unexpected changed-leader dial address")
				}
				return conn, nil
			})
			t.Cleanup(db.closeQuorum)
			proof, err := db.QuorumProof(t.Context())
			if err == nil || !reflect.DeepEqual(proof, QuorumProof{}) {
				t.Fatalf("changed discovery tuple returned proof: %#v/%v", proof, err)
			}
			waitQuorumOwnerDone(t, peerDone)
			if err := peerError(); err != nil {
				t.Fatal("peer did not complete actual request/response", err)
			}
		})
	}
}
