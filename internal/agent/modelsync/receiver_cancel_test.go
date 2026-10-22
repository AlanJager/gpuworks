package modelsync

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestReceiverStopPreservesCancellationWhileReadingHeader(t *testing.T) {
	receiver, err := StartReceiver(context.Background(), ReceiveOptions{
		BindHost: "127.0.0.1", Token: "token",
		Receive: func(context.Context, io.Reader) error {
			t.Error("incomplete header must not start receiving an archive")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(receiver.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := dialPinned(ctx, receiver.Endpoints, nil, receiver.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, `{"magic":`); err != nil {
		t.Fatal(err)
	}
	receiver.Stop()
	if _, err := receiver.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("receiver stop error = %v, want context cancellation", err)
	}
}

func TestReceiverCancellationDuringPayload(t *testing.T) {
	for _, stop := range []bool{true, false} {
		name := "parent context"
		if stop {
			name = "receiver stop"
		}
		t.Run(name, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{})
			receiver, err := StartReceiver(parent, ReceiveOptions{
				BindHost: "127.0.0.1", Token: "token",
				Receive: func(_ context.Context, r io.Reader) error {
					close(started)
					_, err := io.Copy(io.Discard, r)
					return err
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(receiver.Stop)
			ctx, waitCancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer waitCancel()
			conn, err := dialPinned(ctx, receiver.Endpoints, nil, receiver.Fingerprint)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := writeJSONLine(conn, header{Magic: protocolMagic, Token: "token"}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("receiver did not begin reading the archive")
			}
			if stop {
				receiver.Stop()
			} else {
				cancel()
			}
			if _, err := receiver.Wait(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("receiver error = %v, want context cancellation", err)
			}
			assertReceiverClosed(t, receiver)
		})
	}
}

func TestReceiverHeaderDeadlinePreservesTimeout(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	receiver, err := StartReceiver(parent, ReceiveOptions{
		BindHost: "127.0.0.1", Token: "token",
		Receive: func(context.Context, io.Reader) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(receiver.Stop)
	ctx, waitCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer waitCancel()
	conn, err := dialPinned(ctx, receiver.Endpoints, nil, receiver.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := receiver.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("receiver error = %v, want deadline exceeded", err)
	}
	assertReceiverClosed(t, receiver)
}

func TestReceiverDisconnectPreservesNetworkError(t *testing.T) {
	receiver, err := StartReceiver(context.Background(), ReceiveOptions{
		BindHost: "127.0.0.1", Token: "token",
		Receive: func(_ context.Context, r io.Reader) error {
			_, err := io.Copy(io.Discard, r)
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(receiver.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := dialPinned(ctx, receiver.Endpoints, nil, receiver.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := writeJSONLine(conn, header{Magic: protocolMagic, Token: "token"}); err != nil {
		t.Fatal(err)
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], 10)
	if _, err := conn.Write(append(size[:], 'x')); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if _, err := receiver.Wait(ctx); !errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.Canceled) {
		t.Fatalf("receiver error = %v, want truncated-frame network error", err)
	}
	assertReceiverClosed(t, receiver)
}

func TestReceiverCompletedTransferSurvivesLaterCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	receiver, err := StartReceiver(parent, ReceiveOptions{
		BindHost: "127.0.0.1", Token: "token",
		Receive: func(_ context.Context, r io.Reader) error { _, err := io.Copy(io.Discard, r); return err },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(receiver.Stop)
	ctx, waitCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer waitCancel()
	sent, err := Send(ctx, receiver.Endpoints, nil, "token", receiver.Fingerprint, func(w io.Writer) error {
		_, err := io.WriteString(w, "completed archive")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	result, err := receiver.Wait(ctx)
	if err != nil || result != sent {
		t.Fatalf("completed result = %+v, %v; want %+v", result, err, sent)
	}
	assertReceiverClosed(t, receiver)
}

func assertReceiverClosed(t *testing.T, receiver *Receiver) {
	t.Helper()
	accepted := make(chan error, 1)
	go func() {
		conn, err := receiver.listener.Accept()
		if conn != nil {
			conn.Close()
		}
		accepted <- err
	}()
	select {
	case err := <-accepted:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("receiver listener error = %v, want closed listener", err)
		}
	case <-time.After(time.Second):
		t.Fatal("receiver listener was not released after completion")
	}
}
