package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type nodeUninstallerStub struct {
	uninstall func(context.Context, provision.SSHProbeRequest) error
}

func (s nodeUninstallerStub) InstallSSH(context.Context, provision.SSHInstallRequest) (provision.SSHInstallReport, error) {
	return provision.SSHInstallReport{}, errors.New("unexpected install")
}

func (s nodeUninstallerStub) UninstallAgent(ctx context.Context, req provision.SSHProbeRequest) error {
	return s.uninstall(ctx, req)
}

func nodeDeleteFixture(t *testing.T) (*store.Store, *store.Node) {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := auth.NewSecretbox("node-delete-test")
	if err != nil {
		t.Fatal(err)
	}
	st.SetSecretbox(box)
	node, _, err := st.CreateNodePending("remove-me", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeServerURL(node.UUID, "https://control.example"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetNodeSSHCredential(node.UUID, "compute.example", 2222, "operator", store.NodeSSHSecrets{Password: "secret-test-password"}); err != nil {
		t.Fatal(err)
	}
	image, err := st.UpsertImage(&store.Image{ImageRef: "example/image:node-delete", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImageNode(&store.ImageNode{ImageVersionID: image.ID, NodeID: node.ID, State: store.ImageStatePresent}); err != nil {
		t.Fatal(err)
	}
	return st, node
}

func assertNodeDeleteImageRows(t *testing.T, st *store.Store, node *store.Node, want int) {
	t.Helper()
	rows, err := st.ImageNodesForNode(node.ID)
	if err != nil || len(rows) != want {
		t.Fatalf("image rows = %v, err = %v, want %d rows", rows, err, want)
	}
}

func deleteNodeRequest(h *apiHandlers, uuid, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/nodes/"+uuid+query, nil)
	req = withNodeRecoveryURLParam(req, "uuid", uuid)
	rec := httptest.NewRecorder()
	h.deleteNode(rec, req)
	return rec
}

func TestDeleteNodeUninstallsBeforeRemovingRecords(t *testing.T) {
	st, node := nodeDeleteFixture(t)
	uninstalled, kicked := false, false
	h := apiHandlers{d: Deps{Store: st, NodeInstaller: nodeUninstallerStub{uninstall: func(_ context.Context, req provision.SSHProbeRequest) error {
		if req.Host != "compute.example" || req.Port != 2222 || req.User != "operator" || req.Password != "secret-test-password" || req.ServerURL != "https://control.example" {
			t.Error("uninstall did not receive the node's stored endpoint, credentials and server URL")
		}
		// The token picks this node's agent on a host that runs several.
		if req.Token == "" || req.Token != node.Token {
			t.Error("uninstall did not receive the node's token")
		}
		if _, _, err := st.GetNodeSSHCredential(node.UUID); err != nil {
			t.Errorf("credentials removed before uninstall: %v", err)
		}
		uninstalled = true
		return nil
	}}, KickNode: func(uuid string) {
		if uuid != node.UUID || !uninstalled {
			t.Error("session kicked before uninstall completed")
		}
		kicked = true
	}}}
	rec := deleteNodeRequest(&h, node.UUID, "")
	if rec.Code != http.StatusOK || !uninstalled || !kicked {
		t.Fatalf("status=%d, uninstalled=%v kicked=%v body=%s", rec.Code, uninstalled, kicked, rec.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || len(response) != 2 || response["status"] != "deleted" || response["agent_uninstalled"] != true {
		t.Fatalf("unexpected response: %s (%v)", rec.Body.String(), err)
	}
	if _, err := st.NodeByUUID(node.UUID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("node still exists: %v", err)
	}
	assertNodeDeleteImageRows(t, st, node, 0)
}

func TestDeleteNodeRetainsRecordsWhenUninstallFails(t *testing.T) {
	st, node := nodeDeleteFixture(t)
	h := apiHandlers{d: Deps{Store: st, NodeInstaller: nodeUninstallerStub{uninstall: func(context.Context, provision.SSHProbeRequest) error {
		return errors.New("SSH command failed after stopping the service")
	}}, KickNode: func(string) { t.Error("failed uninstall kicked the session") }}}
	rec := deleteNodeRequest(&h, node.UUID, "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"NODE_UNINSTALL_UNCONFIRMED"`) {
		t.Fatalf("uninstall failure must retain node, got status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, _, err := st.GetNodeSSHCredential(node.UUID); err != nil {
		t.Fatalf("retry credentials lost: %v", err)
	}
	if strings.Contains(rec.Body.String(), "secret-test-password") {
		t.Fatal("response exposed SSH password")
	}
	assertNodeDeleteImageRows(t, st, node, 1)
}

func TestDeleteNodeRequiresExplicitForceWithoutSSH(t *testing.T) {
	st, node := nodeDeleteFixture(t)
	if err := st.DeleteNodeSSHCredential(node.UUID); err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, NodeInstaller: nodeUninstallerStub{uninstall: func(context.Context, provision.SSHProbeRequest) error {
		t.Fatal("uninstall must not run without credentials or for force removal")
		return nil
	}}}}
	if rec := deleteNodeRequest(&h, node.UUID, ""); rec.Code != http.StatusConflict {
		t.Fatalf("normal delete without SSH status=%d", rec.Code)
	}
	rec := deleteNodeRequest(&h, node.UUID, "?force=true")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"agent_uninstalled":false`) {
		t.Fatalf("force removal must disclose no uninstall: %d %s", rec.Code, rec.Body.String())
	}
	assertNodeDeleteImageRows(t, st, node, 0)
}

func TestDeleteNodeForceStillRejectsInstances(t *testing.T) {
	st, node := nodeDeleteFixture(t)
	if _, err := st.CreateInstance(&store.Instance{UUID: "busy", NodeID: node.ID, State: proto.InstRunning}); err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, NodeInstaller: nodeUninstallerStub{uninstall: func(context.Context, provision.SSHProbeRequest) error {
		t.Fatal("busy node was uninstalled")
		return nil
	}}}}
	for _, query := range []string{"", "?force=true"} {
		if rec := deleteNodeRequest(&h, node.UUID, query); rec.Code != http.StatusConflict {
			t.Fatalf("busy node %s status=%d", query, rec.Code)
		}
	}
	assertNodeDeleteImageRows(t, st, node, 1)
}

func TestDeleteNodeValidatesForceAndMissingNode(t *testing.T) {
	st, node := nodeDeleteFixture(t)
	h := apiHandlers{d: Deps{Store: st}}
	for _, query := range []string{"?force=", "?force=maybe", "?force=true&force=false"} {
		if rec := deleteNodeRequest(&h, node.UUID, query); rec.Code != http.StatusBadRequest {
			t.Errorf("%s status=%d", query, rec.Code)
		}
	}
	if rec := deleteNodeRequest(&h, "not-found", "?force=true"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing node status=%d", rec.Code)
	}
}

func TestDeleteNodeRetainsRecordsWhenDatabaseCleanupFails(t *testing.T) {
	st, node := nodeDeleteFixture(t)
	if err := st.DB().Exec(`CREATE TRIGGER reject_node_delete BEFORE DELETE ON nodes BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, NodeInstaller: nodeUninstallerStub{uninstall: func(context.Context, provision.SSHProbeRequest) error { return nil }}}}
	rec := deleteNodeRequest(&h, node.UUID, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("database failure returned %d: %s", rec.Code, rec.Body.String())
	}
	if _, _, err := st.GetNodeSSHCredential(node.UUID); err != nil {
		t.Fatalf("transaction did not preserve credentials: %v", err)
	}
	assertNodeDeleteImageRows(t, st, node, 1)
}

func TestDeleteNodeRouteRequiresAuthentication(t *testing.T) {
	st, node := nodeDeleteFixture(t)
	router := Router(Deps{Store: st})
	for _, query := range []string{"", "?force=true"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/v1/nodes/%s%s", node.UUID, query), nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated delete status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
}

func TestNodeDeleteKeepsAgentWhileCacheDeletionUnconfirmed(t *testing.T) {
	st, node := nodeDeleteFixture(t)
	model, err := st.UpsertModel(&store.Model{Source: "local", SourceID: "pending-removal", Name: "pending-removal"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFDeleting, Path: "/models/pending"}); err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, NodeInstaller: nodeUninstallerStub{uninstall: func(context.Context, provision.SSHProbeRequest) error {
		t.Error("agent uninstalled before pending deletion check")
		return nil
	}}}}
	for _, query := range []string{"", "?force=true"} {
		rec := deleteNodeRequest(&h, node.UUID, query)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	if _, err := st.NodeByUUID(node.UUID); err != nil {
		t.Fatal(err)
	}
	assertNodeDeleteImageRows(t, st, node, 1)
}

func TestNodeDeleteReservesCacheBeforeUninstall(t *testing.T) {
	st, node := nodeDeleteFixture(t)
	h := apiHandlers{d: Deps{Store: st, NodeInstaller: nodeUninstallerStub{uninstall: func(context.Context, provision.SSHProbeRequest) error {
		if release, ok := st.TryEvictNodeCache(node.ID); ok {
			release()
			t.Error("cache deletion can start during agent uninstall")
		}
		return errors.New("uninstall interrupted")
	}}}}
	rec := deleteNodeRequest(&h, node.UUID, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d", rec.Code)
	}
	if release, ok := st.TryEvictNodeCache(node.ID); !ok {
		t.Fatal("uninstall error leaked cache reservation")
	} else {
		release()
	}
}

func TestNodeDeleteBusyCacheReturnsProductError(t *testing.T) {
	st, node := nodeDeleteFixture(t)
	release, ok := st.TryPinNodeCaches([]uint{node.ID})
	if !ok {
		t.Fatal("could not reserve node cache for launch")
	}
	defer release()
	h := apiHandlers{d: Deps{Store: st, NodeInstaller: nodeUninstallerStub{uninstall: func(context.Context, provision.SSHProbeRequest) error {
		t.Fatal("busy node was uninstalled")
		return nil
	}}}}
	for _, query := range []string{"", "?force=true"} {
		rec := deleteNodeRequest(&h, node.UUID, query)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusConflict || len(body) != 2 || body["code"] != "NODE_DELETE_CACHE_BUSY" || body["error"] != "node cache is busy; retry node deletion later" {
			t.Errorf("%s status=%d body=%s", query, rec.Code, rec.Body.String())
		}
	}
	if _, err := st.NodeByUUID(node.UUID); err != nil {
		t.Fatal(err)
	}
	assertNodeDeleteImageRows(t, st, node, 1)
}
