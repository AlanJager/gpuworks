package cluster

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type quorumHeldCloseConn struct {
	net.Conn
	entered chan struct{}
	release chan struct{}
	failure error
	calls   atomic.Int32
}

func (c *quorumHeldCloseConn) Close() error {
	c.calls.Add(1)
	actual := c.Conn.Close()
	close(c.entered)
	<-c.release
	return errors.Join(actual, c.failure)
}

func TestReadQuorumProofCancellationJoinsActualHeldClose(t *testing.T) {
	client, server := net.Pipe()
	closeFailure := errors.New("actual connection close acknowledgement failed")
	conn := &quorumHeldCloseConn{Conn: client, entered: make(chan struct{}), release: make(chan struct{}), failure: closeFailure}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(conn.release) }) }
	t.Cleanup(func() { release(); _ = client.Close(); _ = server.Close() })
	requestRead := make(chan error, 1)
	peerClosed := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(server, make([]byte, 24))
		requestRead <- err
		_, err = server.Read(make([]byte, 1))
		peerClosed <- err
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		proof QuorumProof
		err   error
	}
	finished := make(chan result, 1)
	go func() {
		proof, err := ReadQuorumProof(ctx, "node.example:9001", func(context.Context, string) (net.Conn, error) { return conn, nil })
		finished <- result{proof, err}
	}()
	if err := <-requestRead; err != nil {
		t.Fatal(err)
	}
	cancel()
	<-conn.entered
	if err := <-peerClosed; !errors.Is(err, io.EOF) {
		t.Fatalf("cancellation did not close the actual peer stream: %v", err)
	}
	select {
	case got := <-finished:
		t.Fatalf("returned before actual Close completed: %#v", got)
	default:
	}
	release()
	got := <-finished
	if !errors.Is(got.err, context.Canceled) || !errors.Is(got.err, closeFailure) || !reflect.DeepEqual(got.proof, QuorumProof{}) {
		t.Fatalf("cancel/close cause or zero proof lost: %#v", got)
	}
	if conn.calls.Load() != 1 {
		t.Fatalf("actual Close called %d times", conn.calls.Load())
	}
}

type quorumDeadlineConn struct {
	net.Conn
	deadline time.Time
	failure  error
}

func (c *quorumDeadlineConn) SetDeadline(deadline time.Time) error {
	c.deadline = deadline
	if c.failure != nil {
		return c.failure
	}
	return c.Conn.SetDeadline(deadline)
}

func TestReadQuorumProofActualDeadlineAndFiveSecondBound(t *testing.T) {
	t.Run("parent deadline stalls body", func(t *testing.T) {
		client, server := net.Pipe()
		defer server.Close()
		conn := &quorumDeadlineConn{Conn: client}
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		deadline, _ := ctx.Deadline()
		read := make(chan error, 1)
		go func() {
			_, err := io.ReadFull(server, make([]byte, 24))
			read <- err
		}()
		proof, err := ReadQuorumProof(ctx, "node.example:9001", func(context.Context, string) (net.Conn, error) { return conn, nil })
		if err == nil || !reflect.DeepEqual(proof, QuorumProof{}) {
			t.Fatalf("stalled peer returned proof: %#v %v", proof, err)
		}
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("actual deadline cause lost: %v", err)
		}
		if !conn.deadline.Equal(deadline) {
			t.Fatalf("parent deadline changed: got %v want %v", conn.deadline, deadline)
		}
		if err := <-read; err != nil {
			t.Fatal(err)
		}
	})
	t.Run("default cap reaches actual connection", func(t *testing.T) {
		dial, done := quorumFixtureDial(t, quorumFixtureSuccess, false)
		var conn *quorumDeadlineConn
		start := time.Now()
		proof, err := ReadQuorumProof(t.Context(), "node.example:9001", func(ctx context.Context, address string) (net.Conn, error) {
			actual, err := dial(ctx, address)
			if err != nil {
				return actual, err
			}
			conn = &quorumDeadlineConn{Conn: actual}
			return conn, nil
		})
		if err != nil || proof.Index != 21 {
			t.Fatalf("bounded successful request failed: %#v %v", proof, err)
		}
		if conn.deadline.After(time.Now().Add(5*time.Second)) || conn.deadline.Before(start) {
			t.Fatalf("deadline exceeds the fixed cap: %v", conn.deadline)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

type quorumFailingConn struct {
	net.Conn
	writeFailure error
	closeFailure error
}

func (c *quorumFailingConn) Write(body []byte) (int, error) {
	if c.writeFailure != nil {
		n, actual := c.Conn.Write(body[:len(body)-1])
		return n, errors.Join(actual, c.writeFailure)
	}
	return c.Conn.Write(body)
}

func (c *quorumFailingConn) Close() error { return errors.Join(c.Conn.Close(), c.closeFailure) }

func TestReadQuorumProofRetainsActualOperationCauses(t *testing.T) {
	t.Run("partial handshake write and close", func(t *testing.T) {
		client, server := net.Pipe()
		defer server.Close()
		writeFailure := errors.New("peer write failed after partial handshake")
		closeFailure := errors.New("connection cleanup failed")
		conn := &quorumFailingConn{Conn: client, writeFailure: writeFailure, closeFailure: closeFailure}
		peer := make(chan error, 1)
		go func() { _, err := io.ReadFull(server, make([]byte, 8)); peer <- err }()
		proof, err := ReadQuorumProof(t.Context(), "node.example:9001", func(context.Context, string) (net.Conn, error) { return conn, nil })
		if !errors.Is(err, writeFailure) || !errors.Is(err, io.ErrShortWrite) || !errors.Is(err, closeFailure) || !reflect.DeepEqual(proof, QuorumProof{}) {
			t.Fatalf("actual short Write/Close causes were lost: %#v %v", proof, err)
		}
		if err := <-peer; !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("fixture did not observe the actual partial write: %v", err)
		}
	})
	t.Run("close failure withdraws parsed proof", func(t *testing.T) {
		dial, done := quorumFixtureDial(t, quorumFixtureSuccess, false)
		closeFailure := errors.New("parsed proof connection cleanup failed")
		proof, err := ReadQuorumProof(t.Context(), "node.example:9001", func(ctx context.Context, address string) (net.Conn, error) {
			conn, err := dial(ctx, address)
			if err != nil {
				return conn, err
			}
			return &quorumFailingConn{Conn: conn, closeFailure: closeFailure}, nil
		})
		if !errors.Is(err, closeFailure) || !reflect.DeepEqual(proof, QuorumProof{}) {
			t.Fatalf("proof survived cleanup failure: %#v %v", proof, err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
	t.Run("SetDeadline fails before request", func(t *testing.T) {
		client, server := net.Pipe()
		defer server.Close()
		failure := errors.New("connection SetDeadline failed")
		proof, err := ReadQuorumProof(t.Context(), "node.example:9001", func(context.Context, string) (net.Conn, error) {
			return &quorumDeadlineConn{Conn: client, failure: failure}, nil
		})
		if !errors.Is(err, failure) || !reflect.DeepEqual(proof, QuorumProof{}) {
			t.Fatalf("SetDeadline cause lost: %#v %v", proof, err)
		}
		if n, err := server.Read(make([]byte, 24)); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("failed SetDeadline emitted request: n=%d err=%v", n, err)
		}
	})
}

func TestReadQuorumProofDialAdmissionFailures(t *testing.T) {
	t.Run("cancelled before dial", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		calls := 0
		proof, err := ReadQuorumProof(ctx, "node.example:9001", func(context.Context, string) (net.Conn, error) {
			calls++
			return nil, errors.New("unexpected dial")
		})
		if !errors.Is(err, context.Canceled) || calls != 0 || !reflect.DeepEqual(proof, QuorumProof{}) {
			t.Fatalf("cancelled request reached dial: proof=%#v err=%v calls=%d", proof, err, calls)
		}
	})
	t.Run("nil dial", func(t *testing.T) {
		if proof, err := ReadQuorumProof(t.Context(), "node.example:9001", nil); err == nil || !reflect.DeepEqual(proof, QuorumProof{}) {
			t.Fatalf("nil dial returned proof: %#v %v", proof, err)
		}
	})
	t.Run("nil connection", func(t *testing.T) {
		proof, err := ReadQuorumProof(t.Context(), "node.example:9001", func(context.Context, string) (net.Conn, error) { return nil, nil })
		if err == nil || !reflect.DeepEqual(proof, QuorumProof{}) {
			t.Fatalf("nil connection returned proof: %#v %v", proof, err)
		}
	})
	t.Run("failed dial closes returned connection", func(t *testing.T) {
		client, server := net.Pipe()
		defer server.Close()
		dialFailure := errors.New("authenticated dial rejected the peer")
		closeFailure := errors.New("failed dial cleanup acknowledgement")
		proof, err := ReadQuorumProof(t.Context(), "node.example:9001", func(context.Context, string) (net.Conn, error) {
			return &quorumFailingConn{Conn: client, closeFailure: closeFailure}, dialFailure
		})
		if !errors.Is(err, dialFailure) || !errors.Is(err, closeFailure) || !reflect.DeepEqual(proof, QuorumProof{}) {
			t.Fatalf("failed dial cleanup lost cause: %#v %v", proof, err)
		}
		if n, err := server.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("failed dial connection remained live: n=%d err=%v", n, err)
		}
	})
}
