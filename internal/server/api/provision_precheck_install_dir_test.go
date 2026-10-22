package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provision"
)

// installDirPrechecker answers with the rows the real probe produces for the
// RunPod NFS case, so the test pins their wire form.
type installDirPrechecker struct{ req provision.SSHPrecheckRequest }

func (p *installDirPrechecker) PrecheckSSH(_ context.Context, req provision.SSHPrecheckRequest) (provision.SSHPrecheckReport, error) {
	p.req = req
	return provision.SSHPrecheckReport{Target: req.Target, User: "root", Host: "203.0.113.7", Port: 51223, Checks: []provision.CheckResult{
		{Name: "Install directory", Status: "ok", Detail: "/workspace/gpuworks: filesystem nfs4"},
		{Name: "Install directory ownership", Status: "warn", Detail: "udocker workloads will run as the agent's own user"},
		{Name: "Install directory persistence", Status: "error"},
	}}, nil
}

func TestPrecheckNodeAcceptsInstallDirAndReportsItsChecks(t *testing.T) {
	fake := &installDirPrechecker{}
	h := apiHandlers{d: Deps{NodePrecheck: fake}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/precheck", bytes.NewReader([]byte(`{"target":"root@203.0.113.7:51223","auth_mode":"password","password":"pw","server_url":"https://gpuworks.example","install_dir":"/workspace/gpuworks","instance_kind":"container","connect_mode":"agent_dial"}`)))
	rec := httptest.NewRecorder()
	h.precheckNode(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if fake.req.InstallDir != "/workspace/gpuworks" || fake.req.InstanceKind != "container" || fake.req.Target != "root@203.0.113.7:51223" {
		t.Fatalf("request = %+v", fake.req)
	}
	want := `{"target":"root@203.0.113.7:51223","user":"root","host":"203.0.113.7","port":51223,"checks":[` +
		`{"name":"Install directory","status":"ok","detail":"/workspace/gpuworks: filesystem nfs4"},` +
		`{"name":"Install directory ownership","status":"warn","detail":"udocker workloads will run as the agent's own user"},` +
		`{"name":"Install directory persistence","status":"error"}]}`
	if got := bytes.TrimSpace(rec.Body.Bytes()); string(got) != want {
		t.Fatalf("wire JSON =\n%s\nwant\n%s", got, want)
	}
}

// Omitting install_dir is what the wizard sends for the default directory.
func TestPrecheckNodeWithoutInstallDirLeavesTheDefaultToTheProbe(t *testing.T) {
	fake := &installDirPrechecker{}
	h := apiHandlers{d: Deps{NodePrecheck: fake}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/nodes/precheck", bytes.NewReader([]byte(`{"target":"root@203.0.113.7","password":"pw"}`)))
	rec := httptest.NewRecorder()
	h.precheckNode(rec, req)
	if rec.Code != http.StatusOK || fake.req.InstallDir != "" {
		t.Fatalf("status %d, install dir %q", rec.Code, fake.req.InstallDir)
	}
}
