package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

var laneTestDBSeq atomic.Int64

// newTestHub opens a private in-memory store and returns a Hub over it, the
// same wiring the other hub tests use inline.
func newTestHub(t *testing.T) *Hub {
	t.Helper()
	name := fmt.Sprintf("file:hub_lane_test_%d?mode=memory&cache=shared", laneTestDBSeq.Add(1))
	st, err := store.Open(name, bus.New())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return New(st, slog.Default())
}

// newTestNode creates a pending node and returns its UUID and install token.
func newTestNode(t *testing.T, st *store.Store) (uuid, token string) {
	t.Helper()
	n, tok, err := st.CreateNodePending("lane-node")
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	return n.UUID, tok
}

// readEnvelopeInto decodes the next ctrl envelope, asserts its type, and
// unmarshals its payload into v.
func readEnvelopeInto(t *testing.T, dec *json.Decoder, wantT string, v any) {
	t.Helper()
	var env proto.Envelope
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("read %s envelope: %v", wantT, err)
	}
	if env.T != wantT {
		t.Fatalf("envelope t = %q, want %q (d=%s)", env.T, wantT, env.D)
	}
	if v != nil && len(env.D) > 0 {
		if err := json.Unmarshal(env.D, v); err != nil {
			t.Fatalf("decode %s payload: %v", wantT, err)
		}
	}
}
