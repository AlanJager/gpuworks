package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/batch"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/usagereport"
)

func TestMonitorUsesLiveConnectionWithoutDiscardingRunningObservation(t *testing.T) {
	fx := newMonitorFixtureBase(t)
	for _, tc := range []struct {
		name, stored, want string
		live               bool
		ready              int
	}{
		{"before reconnect", store.NodeStateOnline, "offline", false, 0},
		{"authenticated before status write", store.NodeStateOffline, "running", true, 1},
		{"disconnected", store.NodeStateOffline, "offline", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := fx.st.SetNodeState(fx.node.UUID, tc.stored); err != nil {
				t.Fatal(err)
			}
			svc := newMonitorAPIServiceOn(nil, fx.st, fx.ts, func() time.Time { return fx.now }, nil, nil, nil, func(string) bool { return tc.live }).At(fx.now)
			detail, err := svc.Deployment(t.Context(), fx.dep.ID, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			overview, err := svc.Overview(t.Context(), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if len(overview.Services) != 1 || overview.Services[0].State != tc.want || detail.Deployment.Replicas.Ready != tc.ready {
				t.Fatalf("monitor state=%+v replicas=%+v", overview.Services, detail.Deployment.Replicas)
			}
			observed, err := fx.st.InstanceByUUID(fx.inst.UUID)
			if err != nil || observed.State != "running" {
				t.Fatalf("running observation=%+v err=%v", observed, err)
			}
		})
	}
}

func TestUsagePreviewCountsAuthenticatedSessionsRatherThanOldOnlineRows(t *testing.T) {
	rig := newUsageRig(t)
	if _, err := rig.store.RegisterNode("old-online", store.HashToken("token"), "old-online"); err != nil {
		t.Fatal(err)
	}
	if err := rig.store.SetNodeState("old-online", store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	rig.usage.onlineNodes = func() int { return 0 }
	raw, err := rig.usage.Preview(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var report usagereport.Report
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.Environment.Nodes != 1 || report.Environment.OnlineNodes != 0 {
		t.Fatalf("preview environment=%+v", report.Environment)
	}
}

func TestAIAskHTTPSeedsTheCurrentAuthenticatedConnectionState(t *testing.T) {
	f := newAIFixture(t, "ai_live_state_http")
	live := false
	f.ai.online = func(string) bool { return live }
	llm := &scriptedLLM{turns: []string{finalTurn, finalTurn}}
	f.configureLLM(t, llm)
	if _, _, _, err := f.st.EnsureAdmin("admin", "gpuworks-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("ai-live-state-signer", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	router := api.Router(api.Deps{Store: f.st, AuthSigner: signer, AI: f.ai, Online: f.ai.online})
	login := httptest.NewRecorder()
	router.ServeHTTP(login, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"gpuworks-test-password-2026"}`)))
	var session struct{ Token string }
	if login.Code != http.StatusOK || json.Unmarshal(login.Body.Bytes(), &session) != nil || session.Token == "" {
		t.Fatalf("login status=%d", login.Code)
	}
	for _, tc := range []struct {
		stored, want string
		live         bool
	}{
		{store.NodeStateOnline, "connecting", false},
		{store.NodeStateOffline, "online", true},
	} {
		live = tc.live
		if err := f.st.SetNodeState(f.node.UUID, tc.stored); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/ai/ask", strings.NewReader(`{"task":"`+aihelper.TaskNodeHealth+`","node_uuid":"`+f.node.UUID+`","question":"check connection"}`))
		req.Header.Set("Authorization", "Bearer "+session.Token)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Code != http.StatusOK {
			t.Fatalf("ask=%d %s", response.Code, response.Body)
		}
		llm.mu.Lock()
		latest := llm.reqs[len(llm.reqs)-1]
		llm.mu.Unlock()
		seed := ""
		for _, message := range latest.Messages {
			if message.Role == "user" {
				seed += message.Content
			}
		}
		if !strings.Contains(seed, "\nstate: "+tc.want+"\n") {
			t.Fatalf("AI seed lacks state %s: %s", tc.want, seed)
		}
	}
}

func TestBatchModelListAndLookupAgreeOnAuthenticatedConnections(t *testing.T) {
	st := newMonitorTestStore(t)
	addDeployment(t, st, "batch-live", store.DeployActive, false)
	live := false
	models := batchModels{st: st, lists: deployment.NewListsWithConnections(st, nil, nil, func(string) bool { return live })}
	for _, tc := range []struct {
		live bool
		want batch.ModelState
	}{
		{false, batch.ModelOffline}, {true, batch.ModelRunning}, {false, batch.ModelOffline},
	} {
		live = tc.live
		list, err := models.List()
		if err != nil || len(list) != 1 || list[0].State != tc.want {
			t.Fatalf("list=%+v err=%v want=%v", list, err, tc.want)
		}
		found, err := models.Lookup("batch-live")
		if err != nil || found.State != tc.want {
			t.Fatalf("lookup=%+v err=%v want=%v", found, err, tc.want)
		}
	}
}
