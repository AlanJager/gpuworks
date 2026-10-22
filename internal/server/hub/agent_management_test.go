package hub

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	agentconn "gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestPerNodeTokenHandshakePersistsAgentInfo(t *testing.T) {
	st, err := store.Open("file:hub_agent_management_handshake?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	n, token, err := st.CreateNodePending("managed-node")
	if err != nil {
		t.Fatal(err)
	}
	h := New(st, slog.Default())
	ts := httptest.NewServer(hServe(h))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		c := agentconn.New(agentconn.Credentials{ServerURL: ts.URL, NodeToken: token}, func(uuid, issued string) error {
			if uuid != n.UUID || issued != token {
				t.Errorf("registered identity = %q/%q, want %q/%q", uuid, issued, n.UUID, token)
			}
			return nil
		}, noopHandlers{}, emptyProber{}, slog.Default())
		done <- c.Run(ctx)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !h.Online(n.UUID) {
		time.Sleep(10 * time.Millisecond)
	}
	if !h.Online(n.UUID) {
		t.Fatal("per-node token did not establish an online session")
	}
	got, err := st.NodeByUUID(n.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.NodeStateOnline || got.Name != "managed-node" || got.AgentVersion == "" || got.LastRemoteAddr == "" {
		t.Fatalf("node info after handshake = %+v", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not stop")
	}
}

func TestRejectedTokenReturnsExplicitRejectError(t *testing.T) {
	st, err := store.Open("file:hub_agent_management_reject?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	h := New(st, slog.Default())
	ts := httptest.NewServer(hServe(h))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := agentconn.New(agentconn.Credentials{ServerURL: ts.URL, NodeToken: "wrong-token"}, nil, noopHandlers{}, emptyProber{}, slog.Default())
	err = c.Run(ctx)
	var rejected *agentconn.RejectError
	if !errors.As(err, &rejected) || rejected.Reason != "bad_token" {
		t.Fatalf("Run error = %v, want bad_token RejectError", err)
	}
}

func TestRejectedTokenDoesNotTrustUnverifiedUUID(t *testing.T) {
	st, err := store.Open("file:hub_agent_management_reject_uuid?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	n, _, err := st.CreateNodePending("known-node")
	if err != nil {
		t.Fatal(err)
	}
	h := New(st, slog.Default())
	ts := httptest.NewServer(hServe(h))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := agentconn.New(agentconn.Credentials{ServerURL: ts.URL, NodeUUID: n.UUID, NodeToken: "wrong-token"}, nil, noopHandlers{}, emptyProber{}, slog.Default())
	var rejected *agentconn.RejectError
	if err := c.Run(ctx); !errors.As(err, &rejected) || rejected.Reason != "bad_token" {
		t.Fatalf("Run error = %v, want bad_token RejectError", err)
	}
	got, err := st.NodeByUUID(n.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastRejectReason != "" || got.LastRejectAt != nil {
		t.Fatalf("unverified UUID polluted reject info = %q/%v", got.LastRejectReason, got.LastRejectAt)
	}
	events := h.RecentRejects()
	if len(events) == 0 || events[0].Reason != "bad_token" || events[0].TokenPrefix != "wrong-to" {
		t.Fatalf("recent reject events = %+v", events)
	}
}

func TestSmokeHandshakeDoesNotRegisterSession(t *testing.T) {
	st, err := store.Open("file:hub_agent_management_smoke?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := st.CreateNodePending("smoke-node")
	if err != nil {
		t.Fatal(err)
	}
	h := New(st, slog.Default())
	ts := httptest.NewServer(hServe(h))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := agentconn.New(agentconn.Credentials{ServerURL: ts.URL, NodeToken: token, Smoke: true}, nil, nil, emptyProber{}, slog.Default())
	if err := c.Run(ctx); err != nil {
		t.Fatalf("smoke Run: %v", err)
	}
	if h.SessionCount() != 0 {
		t.Fatalf("smoke registered %d sessions", h.SessionCount())
	}
}

func hServe(h *Hub) http.HandlerFunc { return h.ServeWS }
