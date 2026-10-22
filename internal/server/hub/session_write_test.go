package hub

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

type recordingDeadlineConn struct {
	net.Conn
	deadlines []time.Time
}

func (c *recordingDeadlineConn) SetWriteDeadline(deadline time.Time) error {
	c.deadlines = append(c.deadlines, deadline)
	return c.Conn.SetWriteDeadline(deadline)
}

func TestCtrlSendClearsDeadlineAfterEachCompleteEnvelope(t *testing.T) {
	left, right := net.Pipe()
	conn := &recordingDeadlineConn{Conn: left}
	h := newTestHub(t)
	now := time.Now().Add(time.Hour)
	h.now = func() time.Time { return now }
	s := newSession(h, nil, conn)
	t.Cleanup(func() { _ = right.Close(); s.Close() })
	received := make(chan proto.Envelope, 2)
	go func() {
		dec := json.NewDecoder(right)
		for range 2 {
			var env proto.Envelope
			if dec.Decode(&env) != nil {
				return
			}
			received <- env
		}
	}()
	for id := uint64(1); id <= 2; id++ {
		if err := s.send(proto.TStateUpdate, id, proto.StateUpdate{}); err != nil {
			t.Fatal(err)
		}
		select {
		case env := <-received:
			if env.ID != id {
				t.Fatalf("envelope id=%d want %d", env.ID, id)
			}
		case <-time.After(time.Second):
			t.Fatal("complete envelope was not delivered")
		}
	}
	if len(conn.deadlines) != 4 {
		t.Fatalf("deadline calls=%d want set/clear for both envelopes", len(conn.deadlines))
	}
	for i, deadline := range conn.deadlines {
		if deadline.IsZero() != (i%2 == 1) {
			t.Fatalf("deadline %d not set/cleared correctly: %v", i, deadline)
		}
		if i%2 == 0 && !deadline.Equal(now.Add(30*time.Second)) {
			t.Fatalf("deadline %d = %v, want injected clock + 30s", i, deadline)
		}
	}
	select {
	case <-s.closed:
		t.Fatal("successful writes closed healthy session")
	default:
	}
}
