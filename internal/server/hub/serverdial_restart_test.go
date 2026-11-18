package hub

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agentconf"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestServerDialReconnectAfterRestartDoesNotWaitForPersistedOnlineToExpire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	st, err := store.Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, token, err := st.CreateNodePending("restart-node", agentconf.ServerDial)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeState(node.UUID, store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchNode(node.UUID); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// A new server has no live sessions, even though the last heartbeat and
	// online projection survived in the database. No stale sweep runs here.
	st, err = store.Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(h.waitSessionWork)
	ln := listenForReconnect(t, h, node)
	connections := serveReconnectAgent(t, ln, node, token, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.RunServerDialReconnect(ctx, time.Hour)
	}()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, 3*time.Second, func() bool { return h.Online(node.UUID) }, "fresh server to reconnect a persisted-online node on its first pass")
	// Further passes must leave an authenticated live session alone.
	h.reconnectServerDial(ctx)
	if got := connections.Load(); got != 1 {
		t.Fatalf("accepted connections = %d, want one initial reconnect and no replacement", got)
	}
	got, err := st.NodeByUUID(node.UUID)
	if err != nil || got.TokenHash != store.HashToken(token) || got.State != store.NodeStateOnline {
		t.Fatalf("reconnected node = %+v, err=%v", got, err)
	}
}
