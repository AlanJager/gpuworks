package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestSelectInstanceSaysDeploymentFailedWhenRetriesStopped(t *testing.T) {
	for _, tc := range []struct {
		name    string
		states  []string
		wantSub string
		still   bool
		offline bool
	}{
		{name: "all failed", states: []string{proto.InstFailed, proto.InstFailed}, wantSub: "failed to start"},
		{name: "failed and stopped", states: []string{proto.InstFailed, proto.InstStopped}, wantSub: "failed to start"},
		{name: "retry still starting", states: []string{proto.InstFailed, proto.InstStarting}, wantSub: "no reachable running instance", still: true},
		{name: "no instance yet", states: nil, wantSub: "no reachable running instance", still: true},
		{name: "running engine with disconnected node", states: []string{proto.InstRunning}, wantSub: "no reachable running instance", still: true, offline: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open("file:proxy_gave_up_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := st.Close(); err != nil {
					t.Error(err)
				}
			})
			model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/failed", Revision: "main", Name: "failed"})
			if err != nil {
				t.Fatal(err)
			}
			node, err := st.RegisterNode("failed-node", store.HashToken("node"), "failed-node")
			if err != nil {
				t.Fatal(err)
			}
			dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "failed-model", Engine: "vllm", State: "active"})
			if err != nil {
				t.Fatal(err)
			}
			for i, state := range tc.states {
				if _, err := st.CreateInstance(&store.Instance{
					UUID: state + "-" + string(rune('a'+i)), DeploymentID: dep.ID, NodeID: node.ID, State: state, APIPort: 30001,
				}); err != nil {
					t.Fatal(err)
				}
			}
			p := New(st, storeNetworks(st), readinessTestHub{online: map[string]bool{"failed-node": !tc.offline}}, nil, 0)
			t.Cleanup(p.Close)
			_, _, status, msg, asleep := p.selectStatus("failed-model")
			if status != http.StatusServiceUnavailable || asleep || !strings.Contains(msg, tc.wantSub) {
				t.Fatalf("status=%d message=%q asleep=%v, want 503 containing %q", status, msg, asleep, tc.wantSub)
			}
			if tc.still && strings.Contains(msg, "failed to start") {
				t.Fatalf("message=%q, a live retry must stay on the starting text", msg)
			}
			if !tc.still && strings.Contains(msg, "still starting?") {
				t.Fatalf("message=%q, a stopped failure must not say it is still starting", msg)
			}
			if !tc.still && !strings.Contains(msg, "deployment list") {
				t.Fatalf("message=%q, want a pointer at the deployment list", msg)
			}
			w := httptest.NewRecorder()
			key, err := st.CreateAPIKey("refusal-check", 0)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"failed-model","messages":[{"role":"user","content":"hello"}]}`))
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			p.Handler().ServeHTTP(w, req)
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			wantCode := "DEPLOYMENT_START_FAILED"
			if tc.still {
				wantCode = "MODEL_NOT_READY"
			}
			if w.Code != http.StatusServiceUnavailable || body.Error.Code != wantCode || body.Error.Type != "server_error" || body.Error.Message != msg || strings.Contains(body.Error.Message, "still starting?") {
				t.Fatalf("HTTP refusal = %d %s", w.Code, w.Body.String())
			}
		})
	}
}
