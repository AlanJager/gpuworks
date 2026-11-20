package cluster

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

func socketOwnerFixture(t *testing.T) *quorumSQLSockets {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	owner := &quorumSQLSockets{life: ctx, cancel: cancel}
	t.Cleanup(func() { _ = owner.Close() })
	return owner
}

func TestQuorumSQLSocketOwnerJoinsSDKCloseAndRefusesNewDial(t *testing.T) {
	owner := socketOwnerFixture(t)
	client, peer := net.Pipe()
	failure := errors.New("actual SQL socket Close acknowledgement failed")
	held := &quorumHeldCloseConn{Conn: client, entered: make(chan struct{}), release: make(chan struct{}), failure: failure}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(held.release) }) }
	t.Cleanup(func() { release(); _ = peer.Close() })
	conn, err := owner.dial(t.Context(), "local.example:9001", func(context.Context, string) (net.Conn, error) { return held, nil })
	if err != nil {
		t.Fatal(err)
	}
	closedBySDK := make(chan error, 1)
	go func() { closedBySDK <- conn.Close() }()
	<-held.entered
	if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatal("actual peer stream was not closed", err)
	}
	ownerClosed := make(chan error, 1)
	go func() { ownerClosed <- owner.Close() }()
	// Close of the same captured connection must join its first actual Close.
	duplicate := make(chan error, 1)
	go func() { duplicate <- conn.Close() }()
	select {
	case err := <-duplicate:
		t.Fatal("duplicate Close skipped the actual acknowledgement", err)
	default:
	}
	release()
	for _, result := range []<-chan error{closedBySDK, duplicate, ownerClosed} {
		if err := <-result; !errors.Is(err, failure) {
			t.Fatal("actual Close cause lost", err)
		}
	}
	var calls atomic.Int32
	_, err = owner.dial(t.Context(), "local.example:9001", func(context.Context, string) (net.Conn, error) {
		calls.Add(1)
		return nil, errors.New("should not dial")
	})
	if err == nil || calls.Load() != 0 || held.calls.Load() != 1 {
		t.Fatalf("closed owner admitted dial or repeated actual Close: %v dial=%d close=%d", err, calls.Load(), held.calls.Load())
	}
}

func TestQuorumSQLSocketOwnerJoinsLateActualDialAndClose(t *testing.T) {
	owner := socketOwnerFixture(t)
	client, peer := net.Pipe()
	held := &quorumHeldCloseConn{Conn: client, entered: make(chan struct{}), release: make(chan struct{})}
	dialStarted, returnSocket, dialCanceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var dialOnce, closeOnce sync.Once
	releaseDial := func() { dialOnce.Do(func() { close(returnSocket) }) }
	releaseClose := func() { closeOnce.Do(func() { close(held.release) }) }
	t.Cleanup(func() { releaseDial(); releaseClose(); _ = peer.Close() })
	dialDone, closeDone := make(chan struct{}), make(chan struct{})
	dialError := make(chan error, 1)
	go func() {
		defer close(dialDone)
		conn, err := owner.dial(t.Context(), "local.example:9001", func(ctx context.Context, _ string) (net.Conn, error) {
			close(dialStarted)
			<-ctx.Done()
			close(dialCanceled)
			<-returnSocket
			return held, nil // A late real socket must still be closed and joined.
		})
		if conn != nil {
			err = errors.Join(err, errors.New("late dial escaped its owner"))
		}
		dialError <- err
	}()
	<-dialStarted
	go func() { defer close(closeDone); _ = owner.Close() }()
	<-dialCanceled
	quorumOwnerStillWaiting(t, closeDone)
	releaseDial()
	<-held.entered
	quorumOwnerStillWaiting(t, dialDone)
	quorumOwnerStillWaiting(t, closeDone)
	releaseClose()
	waitQuorumOwnerDone(t, dialDone)
	waitQuorumOwnerDone(t, closeDone)
	if err := <-dialError; !errors.Is(err, context.Canceled) || held.calls.Load() != 1 {
		t.Fatalf("late dial/actual Close ownership lost: %v close=%d", err, held.calls.Load())
	}
}

func TestQuorumSQLSocketDialErrorClosesReturnedOriginal(t *testing.T) {
	owner := socketOwnerFixture(t)
	client, peer := net.Pipe()
	defer peer.Close()
	failure := errors.New("actual dial validation failed")
	conn, err := owner.dial(t.Context(), "local.example:9001", func(context.Context, string) (net.Conn, error) { return client, failure })
	if conn != nil || !errors.Is(err, failure) {
		t.Fatalf("failed dial escaped socket or lost cause: %v %v", conn, err)
	}
	if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatal("returned failed original connection stayed open", err)
	}
}
