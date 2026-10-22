package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A restart of every instance at once removes every instance of the
// deployment, failed attempts included, whatever its shape, and records one
// deployment event.
func TestRestartAllCoversEveryShape(t *testing.T) {
	h, st, created := createPDForOps(t)
	pd := mustDeployment(t, st, created.ID)
	plain, err := st.CreateDeployment(&store.Deployment{Name: "plain", ServedName: "plain", State: "active", NodeID: pd.NodeID, Replicas: 2})
	if err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		dep   uint
		uuid  string
		role  string
		state string
	}{
		{pd.ID, "pd-decode", "decode", proto.InstRunning},
		{pd.ID, "pd-prefill", "prefill", proto.InstRunning},
		{pd.ID, "pd-prefill-failed", "prefill", proto.InstFailed},
		{plain.ID, "plain-0", "", proto.InstRunning},
		{plain.ID, "plain-1", "", proto.InstStarting},
		{plain.ID, "plain-failed", "", proto.InstFailed},
		{plain.ID, "plain-removing", "", proto.InstRemoving},
	}
	for _, row := range rows {
		if _, err := st.CreateInstance(&store.Instance{UUID: row.uuid, DeploymentID: row.dep, NodeID: pd.NodeID, State: row.state, Role: row.role}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		dep       uint
		restarted int
	}{{pd.ID, 3}, {plain.ID, 3}} {
		rec := pdOpsRequest(h.restartDeployment, http.MethodPost, tc.dep, `{"strategy":"all"}`)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("restart %d = %d %s", tc.dep, rec.Code, rec.Body.String())
		}
		var out dto.Deployment
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.ID != tc.dep {
			t.Fatalf("restart %d body = %s (%v)", tc.dep, rec.Body.String(), err)
		}
		event, found, err := st.LatestScheduleEvent(tc.dep)
		if err != nil || !found || event.Code != store.EventDeploymentRestarted || !strings.Contains(event.Data, fmt.Sprintf(`"instances":%d`, tc.restarted)) {
			t.Fatalf("restart %d event = %+v found=%v err=%v", tc.dep, event, found, err)
		}
	}
	for _, row := range rows {
		inst, err := st.InstanceByUUID(row.uuid)
		if err != nil {
			t.Fatal(err)
		}
		// The failed attempts go too, so each replica restarts with its full
		// retry budget.
		if inst.State != proto.InstRemoving {
			t.Fatalf("%s = %s, want removing", row.uuid, inst.State)
		}
	}
}

// A rolling restart is the default: it records when it was asked for, gives
// every replica its budget back by retiring the failed attempts, and leaves
// the running instances to the controller, which replaces them one at a time.
func TestRestartIsRollingByDefault(t *testing.T) {
	h, st, created := createPDForOps(t)
	plain, err := st.CreateDeployment(&store.Deployment{Name: "plain", ServedName: "plain", State: "active", NodeID: mustDeployment(t, st, created.ID).NodeID, Replicas: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []store.Instance{
		{UUID: "roll-0", State: proto.InstRunning},
		{UUID: "roll-1", State: proto.InstRunning},
		{UUID: "roll-failed", State: proto.InstFailed},
	} {
		row.DeploymentID, row.NodeID = plain.ID, plain.NodeID
		if _, err := st.CreateInstance(&row); err != nil {
			t.Fatal(err)
		}
	}
	for _, body := range []string{``, `{}`, `{"strategy":"rolling"}`} {
		rec := pdOpsRequest(h.restartDeployment, http.MethodPost, plain.ID, body)
		if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"restart_requested_at":"`) {
			t.Fatalf("rolling restart %q = %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if dep := mustDeployment(t, st, plain.ID); dep.RestartRequestedAt == nil {
		t.Fatal("no restart recorded")
	}
	if got := instanceStates(t, st, "roll-0", "roll-1", "roll-failed"); got["roll-0"] != proto.InstRunning || got["roll-1"] != proto.InstRunning || got["roll-failed"] != proto.InstRemoving {
		t.Fatalf("states = %v", got)
	}
	event, found, _ := st.LatestScheduleEvent(plain.ID)
	if !found || event.Code != store.EventDeploymentRollingRestarted || !strings.Contains(event.Data, `"replicas":2`) {
		t.Fatalf("event = %+v", event)
	}
	// Restarting all at once ends the rolling restart.
	if rec := pdOpsRequest(h.restartDeployment, http.MethodPost, plain.ID, `{"strategy":"all"}`); rec.Code != http.StatusAccepted || strings.Contains(rec.Body.String(), `"restart_requested_at"`) {
		t.Fatalf("restart all = %d %s", rec.Code, rec.Body.String())
	}
	if dep := mustDeployment(t, st, plain.ID); dep.RestartRequestedAt != nil {
		t.Fatalf("restart all kept the rolling restart: %v", dep.RestartRequestedAt)
	}
}

func TestRestartDeploymentRefusals(t *testing.T) {
	h, st, created := createPDForOps(t)
	if rec := pdOpsRequest(h.restartDeployment, http.MethodPost, 9999, ``); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown = %d %s", rec.Code, rec.Body.String())
	}
	if rec := pdOpsRequest(h.restartDeployment, http.MethodPost, created.ID, `{"strategy":"canary"}`); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"`+ErrorRestartStrategyInvalid+`"`) {
		t.Fatalf("unknown strategy = %d %s", rec.Code, rec.Body.String())
	}
	if rec := pdOpsRequest(h.restartDeployment, http.MethodPost, created.ID, `{"max_unavailable":2}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d %s", rec.Code, rec.Body.String())
	}
	for _, state := range []store.DeploymentState{store.DeployStopped, store.DeployDeleting} {
		if err := st.SetDeploymentState(created.ID, state); err != nil {
			t.Fatal(err)
		}
		rec := pdOpsRequest(h.restartDeployment, http.MethodPost, created.ID, ``)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"`+ErrorDeploymentNotActive+`"`) || !strings.Contains(rec.Body.String(), `"state":"`+string(state)+`"`) {
			t.Fatalf("%s restart = %d %s", state, rec.Code, rec.Body.String())
		}
	}
	if _, found, _ := st.LatestScheduleEvent(created.ID); found {
		t.Fatal("a refused restart recorded an event")
	}
}

// A stopped deployment starts again with its configuration; starting an
// active one changes nothing, and a deployment on its way out cannot start.
func TestStartDeployment(t *testing.T) {
	h, st, created := createPDForOps(t)
	if err := st.SetDeploymentState(created.ID, store.DeployStopped); err != nil {
		t.Fatal(err)
	}
	before := mustDeployment(t, st, created.ID)
	rec := pdOpsRequest(h.startDeployment, http.MethodPost, created.ID, ``)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"desired_state":"active"`) {
		t.Fatalf("start = %d %s", rec.Code, rec.Body.String())
	}
	after := mustDeployment(t, st, created.ID)
	if after.State != store.DeployActive || after.Placement != before.Placement || after.Engine != before.Engine {
		t.Fatalf("started deployment = %+v, before %+v", after, before)
	}
	if rec := pdOpsRequest(h.startDeployment, http.MethodPost, created.ID, ``); rec.Code != http.StatusOK || mustDeployment(t, st, created.ID).State != store.DeployActive {
		t.Fatalf("start active = %d %s", rec.Code, rec.Body.String())
	}
	if err := st.SetDeploymentState(created.ID, store.DeployDeleting); err != nil {
		t.Fatal(err)
	}
	rec = pdOpsRequest(h.startDeployment, http.MethodPost, created.ID, ``)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"`+ErrorDeploymentDeleting+`"`) {
		t.Fatalf("start deleting = %d %s", rec.Code, rec.Body.String())
	}
	if mustDeployment(t, st, created.ID).State != store.DeployDeleting {
		t.Fatal("a refused start changed the state")
	}
	if rec := pdOpsRequest(h.startDeployment, http.MethodPost, 9999, ``); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown = %d", rec.Code)
	}
}

// While the shared storage a deployment reads is unreachable, its running
// instance keeps serving from GPU memory; a restart would remove it with no
// way to start a replacement, so it is refused until the storage is back.
func TestRestartIsRefusedWhileTheSharedStorageIsUnavailable(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("gpu3", store.HashToken("t"), "gpu3")
	if err != nil {
		t.Fatal(err)
	}
	nfs, err := st.CreateStorage(&store.Storage{Name: "team-nfs", Type: proto.StorageNFS, Enabled: true, AllNodes: true,
		Path: "/mnt/gpuworks/team-nfs", Export: "nfs.example.internal:/export/models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	report := func(state string) {
		t.Helper()
		if err := st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{
			{ID: nfs.ID, State: state, Path: "/mnt/gpuworks/team-nfs", Writable: true},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	report(proto.StorageStateUnresponsive)
	model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Qwen/Qwen3-0.6B", Name: "q06"})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "q06", ServedName: "q06", ModelID: model.ID, State: store.DeployActive, NodeID: node.ID, Replicas: 1, SaveToStorageID: nfs.ID})
	if err != nil {
		t.Fatal(err)
	}
	shared, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: nfs.ID, Path: "Qwen/Qwen3-0.6B", State: proto.MFPresent})
	if err != nil {
		t.Fatal(err)
	}
	// A second deployment saving into the same storage whose model the node
	// holds locally: a replacement reads that copy, so the outage does not
	// stop its restart.
	nearModel, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Qwen/Qwen3-1.7B", Name: "near"})
	if err != nil {
		t.Fatal(err)
	}
	nearby, err := st.CreateDeployment(&store.Deployment{Name: "near", ServedName: "near", ModelID: nearModel.ID, State: store.DeployActive, NodeID: node.ID, Replicas: 1, SaveToStorageID: nfs.ID})
	if err != nil {
		t.Fatal(err)
	}
	own, err := st.UpsertModelFile(&store.ModelFile{ModelID: nearModel.ID, StorageID: st.DefaultLocalStorageID(), NodeID: node.ID, Path: "/opt/gpuworks/models/q", State: proto.MFPresent})
	if err != nil {
		t.Fatal(err)
	}
	inst := &store.Instance{UUID: "q06-0", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning}
	if err := st.CreateInstances([]*store.Instance{inst, {UUID: "near-0", DeploymentID: nearby.ID, NodeID: node.ID, State: proto.InstRunning}},
		[][]store.InstanceModelFile{{{ModelID: model.ID, ModelFileID: shared.ID, HostPath: "/mnt/gpuworks/team-nfs/Qwen/Qwen3-0.6B"}}, {{ModelID: nearModel.ID, ModelFileID: own.ID, HostPath: own.Path}}}); err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	if rec := pdOpsRequest(h.restartDeployment, http.MethodPost, nearby.ID, `{"strategy":"all"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("restart of a deployment reading a node copy = %d %s", rec.Code, rec.Body.String())
	}
	for _, strategy := range []string{`{}`, `{"strategy":"all"}`} {
		rec := pdOpsRequest(h.restartDeployment, http.MethodPost, dep.ID, strategy)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrorStorageUnavailableOnNode) || !strings.Contains(rec.Body.String(), `"state":"unresponsive"`) {
			t.Fatalf("restart %s = %d %s", strategy, rec.Code, rec.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("uuid", inst.UUID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))
	rec := httptest.NewRecorder()
	h.restartInstance(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), ErrorStorageUnavailableOnNode) {
		t.Fatalf("instance restart = %d %s", rec.Code, rec.Body.String())
	}
	if got, _ := st.InstanceByUUID(inst.UUID); got.State != proto.InstRunning {
		t.Fatalf("refused restart touched the instance: %s", got.State)
	}
	report(proto.StorageStateReady)
	if rec := pdOpsRequest(h.restartDeployment, http.MethodPost, dep.ID, `{"strategy":"all"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("restart with the storage back = %d %s", rec.Code, rec.Body.String())
	}
}
