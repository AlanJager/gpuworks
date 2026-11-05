package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type connectionDiagnosticStub struct {
	fakeKeyInstaller
	request    provision.SSHProbeRequest
	calls      int
	contextErr error
	result     provision.ConnectionDiagnostic
}

func (s *connectionDiagnosticStub) DiagnoseConnection(ctx context.Context, req provision.SSHProbeRequest) provision.ConnectionDiagnostic {
	s.request = req
	s.calls++
	s.contextErr = ctx.Err()
	return s.result
}

func (*connectionDiagnosticStub) ConnectionExposure() *dto.NodeConnectionExposure {
	return &dto.NodeConnectionExposure{URL: "https://current.example", State: "ready", ObservedAt: time.Date(2026, 10, 23, 10, 17, 24, 0, time.UTC)}
}

func TestNodeConnectionDiagnosticsWireIdentityAndReadOnlyState(t *testing.T) {
	st, _ := platformKeyAPIStore(t, true)
	node, _, err := st.CreateNodePending("connection-test", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	last := time.Date(2026, 10, 22, 5, 43, 0, 0, time.UTC)
	if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Updates(map[string]any{"server_url": "https://saved.example", "install_dir": "/data/node-1", "last_seen_at": last, "recovery_attempts": 10, "recovery_phase": "probing", "last_recovery_error": "agent has no session"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetNodeSSHCredential(node.UUID, "node.example", 2222, "root", store.NodeSSHSecrets{Password: "private-ssh-password"}); err != nil {
		t.Fatal(err)
	}
	before, _ := st.NodeByUUID(node.UUID)
	at := time.Date(2026, 10, 23, 10, 19, 0, 0, time.UTC)
	installer := &connectionDiagnosticStub{result: provision.ConnectionDiagnostic{
		SSH:           provision.ConnectionCheck{Outcome: "connected", ObservedAt: at},
		Configuration: provision.ConnectionCheck{Outcome: "snapshot", Source: "state_file", Target: "https://actual.example", ObservedAt: at},
		Health:        provision.ConnectionCheck{Outcome: "dns", Target: "https://saved.example/healthz", ObservedAt: at},
	}}
	trigger := &recoveryTriggerStub{}
	h := apiHandlers{d: Deps{Store: st, NodeInstaller: installer, Recovery: trigger}}
	req := withNodeRecoveryURLParam(httptest.NewRequest(http.MethodPost, "/", nil), "uuid", node.UUID)
	rec := httptest.NewRecorder()
	h.nodeConnectionDiagnostics(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 12 {
		t.Fatalf("wire keys=%v", got)
	}
	for _, key := range []string{"node_uuid", "checked_at", "connect_mode", "saved_server_url", "last_seen_at", "recovery_phase", "recovery_attempts", "last_probe_at", "last_recovery_error", "exposure", "checks", "guidance"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("missing %s", key)
		}
	}
	if string(got["saved_server_url"]) != `"https://saved.example"` || string(got["recovery_attempts"]) != "10" || !strings.Contains(string(got["exposure"]), `"observed_at":"2026-10-23T10:17:24Z"`) {
		t.Fatalf("lost historical provenance: %s", rec.Body)
	}
	var checks []map[string]any
	_ = json.Unmarshal(got["checks"], &checks)
	if len(checks) != 3 || checks[0]["vantage"] != "control_plane" || checks[1]["source"] != "state_file" || checks[2]["outcome"] != "dns" || checks[2]["target"] != "https://saved.example/healthz" {
		t.Fatalf("checks=%v", checks)
	}
	if !strings.Contains(string(got["guidance"]), "addresses_differ") || !strings.Contains(string(got["guidance"]), "configuration_differs") {
		t.Fatalf("missing comparison guidance: %s", rec.Body)
	}
	if installer.calls != 1 || installer.request.Token != node.Token || installer.request.ServerURL != "https://saved.example" || installer.request.InstallDir != "/data/node-1" || installer.request.Password != "private-ssh-password" {
		t.Fatal("diagnostic did not use the persisted node identity")
	}
	if trigger.uuid != "" || installer.authReq.Host != "" {
		t.Fatal("diagnostic invoked recovery or key authorization")
	}
	after, _ := st.NodeByUUID(node.UUID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("read-only diagnostic changed node state")
	}
	for _, secret := range []string{node.Token, "private-ssh-password", `"password":`, `"private_key_pem":`} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatal("diagnostic response leaked credentials")
		}
	}
}

func TestNodeConnectionDiagnosticsUnavailableAndNoTargetInput(t *testing.T) {
	rig := newTokenRig(t)
	defer rig.st.Close()
	node, _, err := rig.st.CreateNodePending("no-ssh", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/nodes/" + node.UUID + "/connection-diagnostics"
	for _, body := range []string{"", "{}"} {
		rec := rig.do(rig.session, http.MethodPost, path, body)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"outcome":"credentials_unavailable"`) || !strings.Contains(rec.Body.String(), `"exposure":null`) {
			t.Fatalf("unavailable report: %d %s", rec.Code, rec.Body)
		}
	}
	for _, body := range []string{`{"url":"http://other.example"}`, `{broken`, `{} {}`, strings.Repeat("x", 2048)} {
		rec := rig.do(rig.session, http.MethodPost, path, body)
		if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "other.example") {
			t.Fatalf("arbitrary input accepted or echoed: %d %s", rec.Code, rec.Body)
		}
		var response struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || response.Code != "NODE_CONNECTION_DIAGNOSTICS_INPUT" {
			t.Fatalf("invalid input needs a localized stable code: %s (decode=%v)", rec.Body, err)
		}
	}
	if rec := rig.do(rig.session, http.MethodPost, "/api/v1/nodes/missing/connection-diagnostics", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("404=%d", rec.Code)
	}
	if rec := rig.do("", http.MethodPost, path, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("auth=%d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodGet, path, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method=%d", rec.Code)
	}
}

func TestNodeConnectionDiagnosticsStoreFailureHasSafeStableCode(t *testing.T) {
	st, _ := platformKeyAPIStore(t, true)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	req := withNodeRecoveryURLParam(httptest.NewRequest(http.MethodPost, "/", nil), "uuid", "node-1")
	rec := httptest.NewRecorder()
	h.nodeConnectionDiagnostics(rec, req)
	var response struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError || response.Code != "NODE_CONNECTION_SETTINGS_UNAVAILABLE" {
		t.Fatalf("store failure needs a localized stable code: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "database is closed") || strings.Contains(rec.Body.String(), "SELECT") || strings.Contains(rec.Body.String(), `"detail"`) {
		t.Fatalf("raw store error escaped the diagnostic boundary: %s", rec.Body)
	}
}

func TestNodeConnectionDiagnosticsRouteRequiresAdministrator(t *testing.T) {
	st, signer, router := newSessionRouter(t)
	for _, role := range []string{"user", "admin"} {
		token, err := managementSessionTestToken(t, st, signer, auth.Claims{Username: role, Role: role})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/missing/connection-diagnostics", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		want := http.StatusForbidden
		if role == "admin" {
			want = http.StatusNotFound
		}
		if rec.Code != want {
			t.Fatalf("%s=%d body=%s", role, rec.Code, rec.Body)
		}
	}
}

func TestNodeConnectionDiagnosticsUnsafeEvidenceAndCancellation(t *testing.T) {
	st, _ := platformKeyAPIStore(t, true)
	node, _, _ := st.CreateNodePending("cancelled", "agent_dial")
	_, _ = st.SetNodeSSHCredential(node.UUID, "node.example", 22, "root", store.NodeSSHSecrets{})
	installer := &connectionDiagnosticStub{result: provision.ConnectionDiagnostic{SSH: provision.ConnectionCheck{Outcome: "cancelled", Target: "https://user:password@example"}, Configuration: provision.ConnectionCheck{Outcome: "password=secret", Target: "https://example?token=hidden"}, Health: provision.ConnectionCheck{Outcome: "not_checked"}}}
	h := apiHandlers{d: Deps{Store: st, NodeInstaller: installer}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := withNodeRecoveryURLParam(httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx), "uuid", node.UUID)
	rec := httptest.NewRecorder()
	h.nodeConnectionDiagnostics(rec, req)
	if installer.contextErr != context.Canceled || rec.Code != http.StatusOK {
		t.Fatalf("context=%v status=%d", installer.contextErr, rec.Code)
	}
	for _, secret := range []string{"password", "hidden", "secret", node.Token} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("unsafe evidence leaked: %s", rec.Body)
		}
	}
}

func TestNodeConnectionDiagnosticsServerDialAndUnsupportedProbeStayExplicit(t *testing.T) {
	st, _ := platformKeyAPIStore(t, true)
	node, _, err := st.CreateNodePending("reverse-direction", "server_dial")
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.SetNodeSSHCredential(node.UUID, "node.example", 22, "root", store.NodeSSHSecrets{})
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, NodeInstaller: &fakeKeyInstaller{}}}
	req := withNodeRecoveryURLParam(httptest.NewRequest(http.MethodPost, "/", nil), "uuid", node.UUID)
	rec := httptest.NewRecorder()
	h.nodeConnectionDiagnostics(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"outcome":"unsupported"`) || !strings.Contains(rec.Body.String(), `"outcome":"not_applicable"`) || !strings.Contains(rec.Body.String(), `"server_dial_scope"`) {
		t.Fatalf("unsupported reverse diagnostic: %d %s", rec.Code, rec.Body)
	}
}

func TestNodeConnectionDiagnosticsUnreadableCredentialsArePartialEvidence(t *testing.T) {
	st, _ := platformKeyAPIStore(t, true)
	node, _, err := st.CreateNodePending("credential-error", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.SetNodeSSHCredential(node.UUID, "node.example", 22, "root", store.NodeSSHSecrets{Password: "private-password"})
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := auth.NewSecretbox("a-different-encryption-key")
	if err != nil {
		t.Fatal(err)
	}
	st.SetSecretbox(wrong)
	installer := &connectionDiagnosticStub{}
	h := apiHandlers{d: Deps{Store: st, NodeInstaller: installer}}
	req := withNodeRecoveryURLParam(httptest.NewRequest(http.MethodPost, "/", nil), "uuid", node.UUID)
	rec := httptest.NewRecorder()
	h.nodeConnectionDiagnostics(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"outcome":"credential_error"`) || installer.calls != 0 || strings.Contains(rec.Body.String(), "private-password") {
		t.Fatalf("credential partial report: %d %s", rec.Code, rec.Body)
	}
}
