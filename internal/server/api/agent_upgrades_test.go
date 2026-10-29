package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/agentupgrade"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type upgradeAPITransport struct {
	connections map[string]agentupgrade.Connection
}

func (f *upgradeAPITransport) AgentUpgradeConnection(uuid string) (agentupgrade.Connection, bool) {
	c, ok := f.connections[uuid]
	return c, ok
}
func (*upgradeAPITransport) PushAgentUpgrade(context.Context, string, string, string, []byte) error {
	return errors.New("unexpected push: handler must only record intent")
}
func (*upgradeAPITransport) RequestAgentUpgrade(context.Context, string, string) error {
	return errors.New("unexpected pull: handler must only record intent")
}

func newAgentUpgradeAPIRig(t *testing.T) (*imageManifestRun, *agentupgrade.Service) {
	t.Helper()
	r := newImageManifestRun(t)
	tr := &upgradeAPITransport{connections: map[string]agentupgrade.Connection{}}
	for _, uuid := range []string{"upgrade-a", "upgrade-b", "upgrade-offline"} {
		if _, err := r.store.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
		if err := r.store.SetNodeAgentInfo(uuid, "old-build", ""); err != nil {
			t.Fatal(err)
		}
		if err := r.store.UpdateNodeStatus(uuid, proto.NodeStatus{Sys: proto.SysInfo{OS: "linux", Arch: "amd64"}}); err != nil {
			t.Fatal(err)
		}
		if uuid != "upgrade-offline" {
			tr.connections[uuid] = agentupgrade.Connection{SessionID: "old-" + uuid, Version: "old-build", Caps: []string{proto.CapUpgradePush}, Ready: true}
		}
	}
	svc, err := agentupgrade.New(r.store, agentupgrade.Config{TargetVersion: "target-build", ArtifactDir: t.TempDir(), Transport: tr, ReadBinary: func(name string) ([]byte, error) {
		if name != "gpuworks-agent-linux-amd64" {
			return nil, fmt.Errorf("no artifact %s", name)
		}
		return []byte("verified-build-fixture"), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	r.deps.AgentUpgrades = AgentUpgradeHooks{Service: upgradeAPITestAdapter{service: svc}}
	r.router = Router(r.deps)
	return r, svc
}

func previewUpgradeRequest(t *testing.T, r *imageManifestRun) (dto.AgentUpgradePreview, string) {
	t.Helper()
	rec := r.do(http.MethodPost, "/api/v1/agent-upgrades/preview", "application/json", `{"scope":"all"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview %d %s", rec.Code, rec.Body)
	}
	var p dto.AgentUpgradePreview
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(dto.AgentUpgradeCreateRequest{NodeUUIDs: p.NodeUUIDs, ExpectedTargetVersion: p.TargetVersion, ExpectedManifestFingerprint: p.ManifestFingerprint, IdempotencyKey: "api-fixed-request"})
	if err != nil {
		t.Fatal(err)
	}
	return p, string(b)
}

func TestAgentUpgradePreviewCreateWireAndIdempotency(t *testing.T) {
	r, _ := newAgentUpgradeAPIRig(t)
	p, body := previewUpgradeRequest(t, r)
	if p.TargetVersion != "target-build" || len(p.NodeUUIDs) != 3 || len(p.Nodes) != 3 || p.Counts["eligible"] != 2 || p.Counts["offline"] != 1 || p.ManifestFingerprint == "" {
		t.Fatalf("all-platform snapshot lost members: %+v", p)
	}
	// Query filters and page parameters must not turn the explicit all scope into the node page.
	filtered := r.do(http.MethodPost, "/api/v1/agent-upgrades/preview?page=2&q=no-match", "application/json", `{"scope":"all"}`)
	var again dto.AgentUpgradePreview
	if err := json.Unmarshal(filtered.Body.Bytes(), &again); err != nil {
		t.Fatal(err)
	}
	if len(again.NodeUUIDs) != 3 {
		t.Fatalf("all scope leaked current page: %s", filtered.Body)
	}
	create := r.do(http.MethodPost, "/api/v1/agent-upgrades", "application/json", body)
	if create.Code != http.StatusAccepted {
		t.Fatalf("create %d %s", create.Code, create.Body)
	}
	var run dto.AgentUpgradeRun
	if err := json.Unmarshal(create.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.ID == 0 || run.TargetVersion != "target-build" || run.Counts.Total != 3 || run.Counts.Queued != 2 || run.Counts.Skipped != 1 || run.Counts.Succeeded != 0 || run.State != "running" || run.RequestedBy == "" {
		t.Fatalf("run wire=%s", create.Body)
	}
	for _, private := range []string{"Manifest", "manifest", "artifact_dir", "BaselineSessionID", "baseline_session_id", "idempotency_key", "request_fingerprint"} {
		if strings.Contains(create.Body.String(), `"`+private+`"`) {
			t.Fatalf("internal field leaked: %s", create.Body)
		}
	}
	replay := r.do(http.MethodPost, "/api/v1/agent-upgrades", "application/json", body)
	var replayRun dto.AgentUpgradeRun
	_ = json.Unmarshal(replay.Body.Bytes(), &replayRun)
	if replay.Code != http.StatusAccepted || replayRun.ID != run.ID {
		t.Fatalf("replay %d %s", replay.Code, replay.Body)
	}
	changed := strings.Replace(body, "target-build", "other-build", 1)
	if rec := r.do(http.MethodPost, "/api/v1/agent-upgrades", "application/json", changed); rec.Code != http.StatusConflict {
		t.Fatalf("changed replay %d %s", rec.Code, rec.Body)
	}
	list := r.do(http.MethodGet, "/api/v1/agent-upgrades?page=1&page_size=1", "", "")
	var page dto.AgentUpgradeRunPage
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if list.Code != 200 || page.Total != 1 || len(page.Items) != 1 || page.PageSize != 1 {
		t.Fatalf("page %d %s", list.Code, list.Body)
	}
	items := r.do(http.MethodGet, fmt.Sprintf("/api/v1/agent-upgrades/%d/items?page=1&page_size=1&state=queued", run.ID), "", "")
	var ip dto.AgentUpgradeItemPage
	if err := json.Unmarshal(items.Body.Bytes(), &ip); err != nil {
		t.Fatal(err)
	}
	if items.Code != 200 || ip.Total != 2 || len(ip.Items) != 1 || ip.Items[0].Phase != "queued" || ip.Items[0].CurrentVersion != "old-build" || ip.Items[0].TargetVersion != "target-build" {
		t.Fatalf("items %d %s", items.Code, items.Body)
	}
	stop := r.do(http.MethodPost, fmt.Sprintf("/api/v1/agent-upgrades/%d/stop", run.ID), "", "")
	var stopped dto.AgentUpgradeRun
	_ = json.Unmarshal(stop.Body.Bytes(), &stopped)
	if stop.Code != 202 || stopped.Counts.Cancelled != 2 || stopped.Counts.Skipped != 1 || stopped.Counts.Processed != 3 {
		t.Fatalf("stop %d %s", stop.Code, stop.Body)
	}
	stopAgain := r.do(http.MethodPost, fmt.Sprintf("/api/v1/agent-upgrades/%d/stop", run.ID), "", "")
	if stopAgain.Code != 202 {
		t.Fatalf("repeat stop %d %s", stopAgain.Code, stopAgain.Body)
	}
}

func TestAgentUpgradeRoutesAuthAndValidation(t *testing.T) {
	r, _ := newAgentUpgradeAPIRig(t)
	routes := []struct{ method, path, body string }{
		{"POST", "/api/v1/agent-upgrades/preview", `{"scope":"all"}`},
		{"POST", "/api/v1/agent-upgrades", `{}`},
		{"GET", "/api/v1/agent-upgrades", ""},
		{"GET", "/api/v1/agent-upgrades/1", ""},
		{"GET", "/api/v1/agent-upgrades/1/items", ""},
		{"POST", "/api/v1/agent-upgrades/1/stop", ""},
		{"POST", "/api/v1/agent-upgrades/1/continue-preview", ""},
		{"POST", "/api/v1/agent-upgrades/1/items/upgrade-a/release", `{"note":"checked process"}`},
	}
	admin := r.admin
	for _, tc := range routes {
		r.admin = ""
		rec := r.do(tc.method, tc.path, "application/json", tc.body)
		if rec.Code != 401 {
			t.Fatalf("anonymous %s %s=%d", tc.method, tc.path, rec.Code)
		}
	}
	_, viewer := roleTestToken(t, r.store, "user")
	for _, tc := range routes {
		if tc.method == "POST" {
			r.admin = viewer
			rec := r.do(tc.method, tc.path, "application/json", tc.body)
			if rec.Code != 403 {
				t.Fatalf("viewer %s=%d %s", tc.path, rec.Code, rec.Body)
			}
		}
	}
	r.admin = admin
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/api/v1/agent-upgrades/preview", `{}`, 400},
		{"POST", "/api/v1/agent-upgrades/preview", `{"scope":"filtered"}`, 400},
		{"POST", "/api/v1/agent-upgrades/preview", `{"scope":"all","node_uuids":["upgrade-a"]}`, 400},
		{"POST", "/api/v1/agent-upgrades", `{}`, 400},
		{"GET", "/api/v1/agent-upgrades?page=0", "", 400},
		{"GET", "/api/v1/agent-upgrades?page_size=201", "", 400},
		{"GET", "/api/v1/agent-upgrades/no-id", "", 400},
		{"GET", "/api/v1/agent-upgrades/999999", "", 404},
		{"GET", "/api/v1/agent-upgrades/999999/items?state=bogus", "", 400},
		{"POST", "/api/v1/agent-upgrades/999999/stop", "", 404},
		{"DELETE", "/api/v1/agent-upgrades", "", 405},
	} {
		rec := r.do(tc.method, tc.path, "application/json", tc.body)
		if rec.Code != tc.want {
			t.Fatalf("%s %s=%d %s want%d", tc.method, tc.path, rec.Code, rec.Body, tc.want)
		}
	}
}

func TestSingleAgentUpgradeUsesSharedBatchSlot(t *testing.T) {
	r, _ := newAgentUpgradeAPIRig(t)
	rec := r.do(http.MethodPost, "/api/v1/nodes/upgrade-a/upgrade", "", "")
	var out struct {
		Status string `json:"status"`
		RunID  uint   `json:"run_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 202 || out.Status != "upgrading" || out.RunID == 0 {
		t.Fatalf("single %d %s", rec.Code, rec.Body)
	}
	_, body := previewUpgradeRequest(t, r)
	if rec := r.do(http.MethodPost, "/api/v1/agent-upgrades", "application/json", body); rec.Code != 409 {
		t.Fatalf("single slot bypassed %d %s", rec.Code, rec.Body)
	}
}

func TestAgentUpgradeReadTokenCannotWrite(t *testing.T) {
	r, _ := newAgentUpgradeAPIRig(t)
	made := r.do(http.MethodPost, "/api/v1/platform-tokens", "application/json", `{"name":"upgrade-read-only","scope":"read"}`)
	if made.Code != http.StatusCreated {
		t.Fatalf("token %d %s", made.Code, made.Body)
	}
	var token struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(made.Body.Bytes(), &token); err != nil {
		t.Fatal(err)
	}
	r.admin = token.Token
	if rec := r.do(http.MethodGet, "/api/v1/agent-upgrades", "", ""); rec.Code != 200 {
		t.Fatalf("read token GET=%d %s", rec.Code, rec.Body)
	}
	if rec := r.do(http.MethodPost, "/api/v1/agent-upgrades/preview", "application/json", `{"scope":"all"}`); rec.Code != 403 {
		t.Fatalf("read token POST=%d %s", rec.Code, rec.Body)
	}
}

func TestServerUpgradeUsesAgentUpgradeGate(t *testing.T) {
	t.Run("active task blocks installation", func(t *testing.T) {
		r, _ := newAgentUpgradeAPIRig(t)
		_, body := previewUpgradeRequest(t, r)
		if rec := r.do(http.MethodPost, "/api/v1/agent-upgrades", "application/json", body); rec.Code != 202 {
			t.Fatalf("create %d %s", rec.Code, rec.Body)
		}
		server := &upgradingServer{}
		r.deps.Server = server
		r.router = Router(r.deps)
		rec := r.do(http.MethodPost, "/api/v1/updates/server/upload", "application/octet-stream", "archive")
		if rec.Code != 409 || len(server.archives) != 0 {
			t.Fatalf("server upgrade bypassed active run: %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("rejected server installation releases maintenance gate", func(t *testing.T) {
		r, _ := newAgentUpgradeAPIRig(t)
		server := &upgradingServer{err: errors.New("fixture rejected archive")}
		r.deps.Server = server
		r.router = Router(r.deps)
		rec := r.do(http.MethodPost, "/api/v1/updates/server/upload", "application/octet-stream", "archive")
		if rec.Code == 202 {
			t.Fatal("bad archive accepted")
		}
		_, body := previewUpgradeRequest(t, r)
		if got := r.do(http.MethodPost, "/api/v1/agent-upgrades", "application/json", body); got.Code != 202 {
			t.Fatalf("maintenance not released: %d %s", got.Code, got.Body)
		}
	})
	t.Run("accepted server installation blocks new agent task", func(t *testing.T) {
		r, _ := newAgentUpgradeAPIRig(t)
		_, body := previewUpgradeRequest(t, r)
		r.deps.Server = &upgradingServer{}
		r.router = Router(r.deps)
		if rec := r.do(http.MethodPost, "/api/v1/updates/server/upload", "application/octet-stream", "archive"); rec.Code != 202 {
			t.Fatalf("server %d %s", rec.Code, rec.Body)
		}
		if got := r.do(http.MethodPost, "/api/v1/agent-upgrades", "application/json", body); got.Code != 409 {
			t.Fatalf("new task during server restart=%d %s", got.Code, got.Body)
		}
	})
}
