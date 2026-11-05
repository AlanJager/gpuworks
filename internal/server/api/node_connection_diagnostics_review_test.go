package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestNodeConnectionDiagnosticsR9MissingCapabilitiesHookIsUnavailable(t *testing.T) {
	st, _ := platformKeyAPIStore(t, true)
	node, _, err := st.CreateNodePending("missing-capabilities-hook", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	h := apiHandlers{d: Deps{Store: st, Online: func(string) bool { return true },
		CallNode: func(context.Context, string, string, any) (json.RawMessage, error) {
			calls++
			return json.RawMessage(connectionRuntimeWire), nil
		},
	}}
	rec := httptest.NewRecorder()
	h.nodeConnectionDiagnostics(rec, withNodeRecoveryURLParam(httptest.NewRequest(http.MethodPost, "/", nil), "uuid", node.UUID))
	var out dto.NodeConnectionDiagnostics
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || calls != 0 || out.AgentReport != nil || len(out.Checks) != 4 || out.Checks[0].Outcome != "unavailable" {
		t.Fatalf("missing capabilities integration must remain partial/unavailable, not old-agent evidence: status=%d calls=%d checks=%+v", rec.Code, calls, out.Checks)
	}
}

type connectionURLDiagnosticStub struct {
	*connectionDiagnosticStub
	exposureURL string
}

func (s *connectionURLDiagnosticStub) ConnectionExposure() *dto.NodeConnectionExposure {
	return &dto.NodeConnectionExposure{URL: s.exposureURL, State: "ready", ObservedAt: time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)}
}

func TestNodeConnectionDiagnosticsR9StoredURLsNeverExposeNodeToken(t *testing.T) {
	for _, kind := range []string{"literal token", "percent-encoded token", "safe base path"} {
		t.Run(kind, func(t *testing.T) {
			st, _ := platformKeyAPIStore(t, true)
			node, _, err := st.CreateNodePending("stored-url", "agent_dial")
			if err != nil {
				t.Fatal(err)
			}
			var encoded strings.Builder
			for _, b := range []byte(node.Token) {
				_, _ = fmt.Fprintf(&encoded, "%%%02X", b)
			}
			path := node.Token
			if kind == "percent-encoded token" {
				path = encoded.String()
			} else if kind == "safe base path" {
				path = "valid/route-prefix"
			}
			target := "https://control.example/prefix/" + path
			if err := st.DB().Model(&store.Node{}).Where("uuid = ?", node.UUID).Update("server_url", target).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := st.SetNodeSSHCredential(node.UUID, "node.example", 22, "root", store.NodeSSHSecrets{}); err != nil {
				t.Fatal(err)
			}
			before, err := st.NodeByUUID(node.UUID)
			if err != nil || before.ServerURL != target {
				t.Fatal("the exact fixture URL was not stored")
			}
			installer := &connectionURLDiagnosticStub{connectionDiagnosticStub: &connectionDiagnosticStub{result: provision.ConnectionDiagnostic{
				SSH:           provision.ConnectionCheck{Outcome: "connected", Target: target},
				Configuration: provision.ConnectionCheck{Outcome: "snapshot", Target: target},
				Health:        provision.ConnectionCheck{Outcome: "http_ok", Target: target, HTTPStatus: 200},
			}}, exposureURL: target}
			h := apiHandlers{d: Deps{Store: st, NodeInstaller: installer}}
			rec := httptest.NewRecorder()
			h.nodeConnectionDiagnostics(rec, withNodeRecoveryURLParam(httptest.NewRequest(http.MethodPost, "/", nil), "uuid", node.UUID))
			var out dto.NodeConnectionDiagnostics
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if rec.Code != http.StatusOK || installer.calls != 1 {
				t.Fatalf("expected a real stored-node partial report, status=%d calls=%d", rec.Code, installer.calls)
			}
			if strings.Contains(rec.Body.String(), node.Token) || strings.Contains(rec.Body.String(), encoded.String()) {
				t.Fatal("node credential escaped through the full HTTP response")
			}
			if kind == "safe base path" {
				if out.SavedServerURL != target || out.Exposure == nil || out.Exposure.URL != target {
					t.Fatal("safe base path was lost")
				}
			} else if out.SavedServerURL != "" || out.Exposure != nil {
				t.Fatal("credential-bearing saved/exposure URL was not omitted")
			}
			for _, check := range out.Checks[1:] {
				if (kind == "safe base path" && check.Target != target) || (kind != "safe base path" && check.Target != "") {
					t.Fatal("check target did not preserve the same credential boundary")
				}
			}
			after, _ := st.NodeByUUID(node.UUID)
			if after.ServerURL != target || after.Token != node.Token {
				t.Fatal("display sanitization changed the stored configuration")
			}
		})
	}
}
