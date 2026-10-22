package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/recovery"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type recoveryTriggerStub struct {
	uuid    string
	outcome recovery.Outcome
	err     error
}

func TestNodeSSHProbeUsesPersistedNodeIdentity(t *testing.T) {
	st, _ := platformKeyAPIStore(t, true)
	node, _, err := st.CreateNodePending("isolated-node", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Updates(map[string]any{
		"server_url": "http://gw:8800", "install_dir": "/data/isolated-agent",
	}).Error; err != nil {
		t.Fatal(err)
	}
	installer := &fakeKeyInstaller{}
	h := apiHandlers{d: Deps{Store: st, NodeInstaller: installer}}
	req := withNodeRecoveryURLParam(httptest.NewRequest(http.MethodPut, "/", strings.NewReader(
		`{"host":"gpu.example","user":"root","auth_mode":"platform_key","probe":true}`)), "uuid", node.UUID)
	rec := httptest.NewRecorder()
	h.putNodeSSH(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if installer.probeReq.Token != node.Token || installer.probeReq.ServerURL != "http://gw:8800" || installer.probeReq.InstallDir != "/data/isolated-agent" {
		t.Fatal("SSH probe did not receive the node identity and installation directory")
	}
	assertSanitizedSSHResponse(t, rec.Body.String(), node.Token)
}

func (s *recoveryTriggerStub) Trigger(_ context.Context, uuid string) (recovery.Outcome, error) {
	s.uuid = uuid
	return s.outcome, s.err
}

func TestNodeSSHAPIIsWriteOnlyForSecretMaterial(t *testing.T) {
	st, err := store.Open("file:node_ssh_api?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	box, err := auth.NewSecretbox("node-ssh-api")
	if err != nil {
		t.Fatal(err)
	}
	st.SetSecretbox(box)
	node, _, err := st.CreateNodePending("ssh-api", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}

	secret := "must-never-be-returned"
	body := `{"host":"gpu.example","port":2222,"user":"ubuntu","password":"` + secret + `"}`
	put := httptest.NewRequest(http.MethodPut, "/api/v1/nodes/"+node.UUID+"/ssh", bytes.NewBufferString(body))
	put = withNodeRecoveryURLParam(put, "uuid", node.UUID)
	putRec := httptest.NewRecorder()
	h.putNodeSSH(putRec, put)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", putRec.Code, putRec.Body.String())
	}
	assertSanitizedSSHResponse(t, putRec.Body.String(), secret)

	get := httptest.NewRequest(http.MethodGet, "/api/v1/nodes/"+node.UUID+"/ssh", nil)
	get = withNodeRecoveryURLParam(get, "uuid", node.UUID)
	getRec := httptest.NewRecorder()
	h.getNodeSSH(getRec, get)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	assertSanitizedSSHResponse(t, getRec.Body.String(), secret)
	var response map[string]any
	if err := json.Unmarshal(getRec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["host"] != "gpu.example" || response["user"] != "ubuntu" || response["has_password"] != true {
		t.Fatalf("response=%v", response)
	}

	del := httptest.NewRequest(http.MethodDelete, "/api/v1/nodes/"+node.UUID+"/ssh", nil)
	del = withNodeRecoveryURLParam(del, "uuid", node.UUID)
	delRec := httptest.NewRecorder()
	h.deleteNodeSSH(delRec, del)
	if delRec.Code != http.StatusOK || !strings.Contains(delRec.Body.String(), `"status":"deleted"`) {
		t.Fatalf("DELETE status=%d body=%s", delRec.Code, delRec.Body.String())
	}
	getRec = httptest.NewRecorder()
	h.getNodeSSH(getRec, get)
	if getRec.Code != http.StatusNotFound {
		t.Fatalf("GET after DELETE status=%d body=%s", getRec.Code, getRec.Body.String())
	}
}

func TestManualNodeRecoveryProbeReturnsTriggerOutcome(t *testing.T) {
	trigger := &recoveryTriggerStub{outcome: recovery.Outcome{
		NodeUUID: "node-1", Action: "restart", Phase: store.RecoveryPhaseRestarting, Recovered: true,
	}}
	h := apiHandlers{d: Deps{Recovery: trigger}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/node-1/recovery/probe", nil)
	req = withNodeRecoveryURLParam(req, "uuid", "node-1")
	rec := httptest.NewRecorder()
	h.probeNodeRecovery(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST recovery probe status=%d body=%s", rec.Code, rec.Body.String())
	}
	if trigger.uuid != "node-1" {
		t.Fatalf("trigger UUID=%q", trigger.uuid)
	}
	var got recovery.Outcome
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Action != "restart" || got.Phase != store.RecoveryPhaseRestarting || !got.Recovered {
		t.Fatalf("trigger outcome=%+v", got)
	}
}

func assertSanitizedSSHResponse(t *testing.T, body, secret string) {
	t.Helper()
	if strings.Contains(body, secret) || strings.Contains(body, `"password":`) || strings.Contains(body, `"private_key_pem":`) || strings.Contains(body, "creds_cipher") {
		t.Fatalf("SSH response leaked secret material: %s", body)
	}
}

func withNodeRecoveryURLParam(req *http.Request, key, value string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, value)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}
