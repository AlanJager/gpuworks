package hub

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// TestRealServerDialReconnect is opt-in because it requires a real agent
// already listening in server_dial mode. It proves first registration and a
// subsequent reconnect through a fresh Hub using only persisted node state.
func TestRealServerDialReconnect(t *testing.T) {
	addr := os.Getenv("GPUWORKS_REAL_SERVER_DIAL_ADDR")
	nodeToken := os.Getenv("GPUWORKS_REAL_SERVER_DIAL_TOKEN")
	if addr == "" || nodeToken == "" {
		t.Skip("set GPUWORKS_REAL_SERVER_DIAL_ADDR and GPUWORKS_REAL_SERVER_DIAL_TOKEN")
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "gpuworks.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	registered, err := st.RegisterNode("real-server-dial", store.HashToken(nodeToken), "real-server-dial", "server_dial")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Node{}).Where("id = ?", registered.ID).Update("token", nodeToken).Error; err != nil {
		t.Fatal(err)
	}
	h1 := New(st, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := h1.DialAgentWithSecret(ctx, addr, nodeToken); err != nil {
		t.Fatalf("initial server_dial registration: %v", err)
	}

	nodes, err := st.ServerDialNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("registered server_dial nodes = %d, want 1", len(nodes))
	}
	node := nodes[0]
	if node.ServerDialAddress != addr || !h1.Online(node.UUID) {
		t.Fatalf("initial node state = %+v online=%v", node, h1.Online(node.UUID))
	}

	h1.mu.RLock()
	firstSession := h1.sessions[node.UUID]
	h1.mu.RUnlock()
	if firstSession == nil {
		t.Fatal("initial session missing")
	}
	firstSession.Close()
	waitFor(t, 5*time.Second, func() bool { return !h1.Online(node.UUID) }, "initial session close")

	h2 := New(st, slog.Default())
	reconnectCtx, reconnectCancel := context.WithCancel(context.Background())
	defer reconnectCancel()
	go h2.RunServerDialReconnect(reconnectCtx, 200*time.Millisecond)
	waitFor(t, 10*time.Second, func() bool { return h2.Online(node.UUID) }, "persisted server_dial reconnect")

	nodesAfter, err := st.ServerDialNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodesAfter) != 1 || nodesAfter[0].UUID != node.UUID || nodesAfter[0].ServerDialAddress != addr {
		t.Fatalf("node identity/address changed after reconnect: before=%+v after=%+v", node, nodesAfter)
	}
}

func waitFor(t *testing.T, timeout time.Duration, ready func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}
