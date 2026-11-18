package hub

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"gorm.io/gorm"

	agentconn "gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestTokenLookupFailureIsNotAnAuthenticationRejection(t *testing.T) {
	for _, lane := range []string{"ctrl", "data"} {
		t.Run(lane, func(t *testing.T) {
			h := newTestHub(t)
			t.Cleanup(func() { _ = h.store.Close() })
			t.Cleanup(h.waitSessionWork)
			uuid, token := newTestNode(t, h.store)
			srv := httptest.NewServer(hServe(h))
			t.Cleanup(srv.Close)
			var unavailable atomic.Bool
			readErr := errors.New("temporary node token lookup failure")
			if err := h.store.DB().Callback().Query().Before("gorm:query").Register("node_token_lookup_failure", func(tx *gorm.DB) {
				if unavailable.Load() && tx.Statement.Schema != nil && tx.Statement.Schema.Table == "nodes" {
					tx.AddError(readErr)
				}
			}); err != nil {
				t.Fatal(err)
			}
			// Install the callback before opening a session: GORM compiles its
			// callback list in place, while replay reads run concurrently.
			var registered proto.Registered
			if lane == "data" {
				_, registered = dialCtrlLane(t, srv, token)
			}
			unavailable.Store(true)
			if _, err := h.authNodeToken(token); !errors.Is(err, readErr) {
				t.Errorf("token lookup error = %v, want the storage error", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if lane == "ctrl" {
				c := agentconn.New(agentconn.Credentials{ServerURL: srv.URL, NodeToken: token, Smoke: true}, nil, nil, emptyProber{}, slog.Default())
				var rejected *agentconn.RejectError
				if err := c.Run(ctx); err == nil || errors.As(err, &rejected) || ctx.Err() != nil {
					t.Errorf("lookup outage = %v, want a transient connection failure, not RejectError or timeout", err)
				}
			} else {
				ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v1/agent/connect?lane=data", nil)
				if err != nil {
					t.Fatal(err)
				}
				wire := websocket.NetConn(ctx, ws, websocket.MessageBinary)
				t.Cleanup(func() { _ = wire.Close() })
				if err := json.NewEncoder(wire).Encode(proto.DataHello{NodeToken: token, SessionID: registered.SessionID}); err != nil {
					t.Fatal(err)
				}
				var envelope proto.Envelope
				if err := json.NewDecoder(wire).Decode(&envelope); err == nil || ctx.Err() != nil {
					t.Errorf("outage response = %+v, err=%v; want a closed lane, not an authentication rejection or timeout", envelope, err)
				}
				if !h.Online(uuid) {
					t.Error("data lookup failure displaced the authenticated control session")
				}
			}
			if rejects := h.RecentRejects(); len(rejects) != 0 {
				t.Errorf("storage outage recorded as credential rejection: %+v", rejects)
			}
			unavailable.Store(false)
			if lane == "ctrl" {
				c := agentconn.New(agentconn.Credentials{ServerURL: srv.URL, NodeToken: token, Smoke: true}, nil, nil, emptyProber{}, slog.Default())
				if err := c.Run(ctx); err != nil {
					t.Fatalf("unchanged token after store recovery: %v", err)
				}
			} else {
				ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v1/agent/connect?lane=data", nil)
				if err != nil {
					t.Fatal(err)
				}
				wire := websocket.NetConn(ctx, ws, websocket.MessageBinary)
				t.Cleanup(func() { _ = wire.Close() })
				if err := json.NewEncoder(wire).Encode(proto.DataHello{NodeToken: token, SessionID: registered.SessionID}); err != nil {
					t.Fatal(err)
				}
				var reply proto.Envelope
				if err := json.NewDecoder(wire).Decode(&reply); err != nil || reply.T != proto.TDataAck {
					t.Fatalf("data lane after recovery = %+v, err=%v, want data ack", reply, err)
				}
			}
		})
	}
}
