package hub

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

type shortDeadlineConn struct{ net.Conn }

func (c shortDeadlineConn) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		deadline = time.Now().Add(30 * time.Millisecond)
	}
	return c.Conn.SetWriteDeadline(deadline)
}

func TestCtrlSendBoundsBlockedPeerAndClosesOnlyItsSession(t *testing.T) {
	h := newTestHub(t)
	left, right := net.Pipe()
	t.Cleanup(func() { _ = right.Close() })
	stalled := newSession(h, nil, shortDeadlineConn{left})
	otherLeft, otherRight := net.Pipe()
	other := newSession(h, nil, otherLeft)
	stalled.NodeUUID = "reconnected-node"
	other.NodeUUID = stalled.NodeUUID
	h.sessions[other.NodeUUID] = other
	t.Cleanup(func() { _ = otherRight.Close(); other.Close() })
	done := make(chan error, 1)
	go func() { done <- stalled.send(proto.TStateUpdate, 0, proto.StateUpdate{}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked peer was reported as success")
		}
	case <-time.After(time.Second):
		_ = right.Close()
		<-done
		t.Fatal("ctrl send ignored its write deadline")
	}
	select {
	case <-stalled.closed:
	default:
		t.Fatal("failed ctrl write left session reusable")
	}
	select {
	case <-other.closed:
		t.Fatal("one blocked peer closed another session")
	default:
	}
}

func TestNetworkRequestWaitsForInitialReplayAndReadsLatestSetting(t *testing.T) {
	h := newTestHub(t)
	requester, ok := any(h).(interface{ RequestNetworkSync(string) })
	if !ok {
		t.Fatal("hub has no asynchronous scoped network request")
	}
	uuid, _ := newTestNode(t, h.store)
	sent := make(chan proto.Envelope, 4)
	s := &Session{hub: h, NodeUUID: uuid, closed: make(chan struct{}), initialPushDone: make(chan struct{}), caps: []string{"state.update.network"}, sendFn: func(kind string, id uint64, data any) error {
		raw, err := json.Marshal(data)
		sent <- proto.Envelope{T: kind, ID: id, D: raw}
		return err
	}}
	h.sessions[uuid] = s
	t.Cleanup(s.markClosed)
	setProxies(t, h.store, everyNode("http://first.invalid", ""))
	for range 20 {
		requester.RequestNetworkSync(uuid)
	}
	select {
	case <-sent:
		t.Fatal("network arrived before initial baseline")
	default:
	}
	setProxies(t, h.store, everyNode("http://latest.invalid", ""))
	close(s.initialPushDone)
	select {
	case envelope := <-sent:
		var data struct {
			Network proto.NetworkConfig `json:"network"`
		}
		if err := json.Unmarshal(envelope.D, &data); err != nil {
			t.Fatal(err)
		}
		if envelope.T != proto.TStateUpdate || data.Network.HTTPProxy != "http://latest.invalid" {
			t.Fatalf("network update = %s %s", envelope.T, envelope.D)
		}
	case <-time.After(time.Second):
		t.Fatal("network request was lost")
	}
}
