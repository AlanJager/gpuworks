package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

// The dual-lane handshake is a wire contract between server and agent versions
// that may differ, so these tests pin the literal JSON keys and values.

func TestRegisteredSessionIDWire(t *testing.T) {
	raw, err := json.Marshal(Registered{NodeUUID: "n1", NodeToken: "tok", Proto: 1, SessionID: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"node_uuid":"n1","node_token":"tok","proto":1,"session_id":"abc123"}`
	if string(raw) != want {
		t.Fatalf("wire = %s, want %s", raw, want)
	}
	// An old server never sends session_id; the field must be omitted when empty
	// so a new agent against an old server sees exactly the pre-M2 message.
	raw, _ = json.Marshal(Registered{NodeUUID: "n1", NodeToken: "tok", Proto: 1})
	if strings.Contains(string(raw), "session_id") {
		t.Fatalf("empty session_id was not omitted: %s", raw)
	}
	var got Registered
	if err := json.Unmarshal([]byte(`{"node_uuid":"n1","node_token":"tok","proto":1}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "" {
		t.Fatalf("legacy registered decoded session_id=%q, want empty", got.SessionID)
	}
}

func TestDataHelloWire(t *testing.T) {
	raw, err := json.Marshal(DataHello{NodeToken: "tok", SessionID: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"node_token":"tok","session_id":"abc123"}`
	if string(raw) != want {
		t.Fatalf("wire = %s, want %s", raw, want)
	}
	var got DataHello
	if err := json.Unmarshal([]byte(want), &got); err != nil {
		t.Fatal(err)
	}
	if got != (DataHello{NodeToken: "tok", SessionID: "abc123"}) {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestDataLaneEnvelopeWire(t *testing.T) {
	if TDataAck != "data.ack" || RejectStaleSession != "stale_session" || RejectBadToken != "bad_token" {
		t.Fatalf("lane wire constants changed: %q %q %q", TDataAck, RejectStaleSession, RejectBadToken)
	}
	rejD, _ := json.Marshal(Reject{Reason: RejectStaleSession})
	raw, err := json.Marshal(Envelope{T: TReject, D: rejD})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"t":"reject","d":{"reason":"stale_session"}}`
	if string(raw) != want {
		t.Fatalf("reject wire = %s, want %s", raw, want)
	}
	ack, _ := json.Marshal(Envelope{T: TDataAck})
	if string(ack) != `{"t":"data.ack"}` {
		t.Fatalf("ack wire = %s", ack)
	}
}
