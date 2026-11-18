package hub

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	agentconn "gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The database can outlive the process that authenticated its last session.
// Exercise the actual HTTP router and WS/yamux handshake: only the accepted
// token makes a node online, and a rejected hello cannot borrow its UUID.
func TestNodeHTTPStateTracksAuthenticatedSessionAcrossReconnects(t *testing.T) {
	st, signer, login := imageInspectionTestSession(t)
	node, token, err := st.CreateNodePending("returning-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeState(node.UUID, store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchNodesAt(map[string]time.Time{node.UUID: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	h := New(st, slog.Default())
	router := api.Router(api.Deps{Store: st, AuthSigner: signer, Online: h.Online, AgentConnect: h.ServeWS})
	server := httptest.NewServer(router)
	defer server.Close()
	check := func(want string) {
		t.Helper()
		waitFor(t, 5*time.Second, func() bool {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/nodes/"+node.UUID, nil)
			req.Header.Set("Authorization", "Bearer "+login)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			var got dto.Node
			return response.Code == http.StatusOK && json.Unmarshal(response.Body.Bytes(), &got) == nil && got.State == want
		}, "node HTTP state "+want)
	}
	reject := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		conn := agentconn.New(agentconn.Credentials{ServerURL: server.URL, NodeUUID: node.UUID, NodeToken: "wrong-token"}, nil, noopHandlers{}, emptyProber{}, slog.Default())
		var rejected *agentconn.RejectError
		if err := conn.Run(ctx); !errors.As(err, &rejected) || rejected.Reason != "bad_token" {
			t.Fatalf("wrong-token handshake = %v", err)
		}
	}
	connect := func() func() {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		conn := agentconn.New(agentconn.Credentials{ServerURL: server.URL, NodeToken: token}, nil, noopHandlers{}, emptyProber{}, slog.Default())
		go func() { done <- conn.Run(ctx) }()
		stop := func() {
			cancel()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("agent did not stop")
			}
		}
		t.Cleanup(cancel)
		return stop
	}
	check("connecting")
	reject()
	check("connecting")
	if h.SessionCount() != 0 {
		t.Fatal("rejected hello registered a session")
	}
	saved, err := st.NodeByUUID(node.UUID)
	if err != nil || saved.State != store.NodeStateOnline || saved.LastRejectReason != "" {
		t.Fatal("unverified UUID changed persisted node identity/state")
	}
	stop := connect()
	check("online")
	reject()
	check("online")
	if h.SessionCount() != 1 {
		t.Fatalf("session count=%d, want 1", h.SessionCount())
	}
	stop()
	check("offline")
	stop = connect()
	check("online")
	stop()
	check("offline")
}
