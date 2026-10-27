package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// listFixture seeds the shapes the list joins can meet: a plain single-node
// deployment with a running instance, a multi-node one, one still waiting for a
// placement past its timeout, one whose model file is missing, one whose node
// was deleted, and one whose model was deleted.
type listFixture struct {
	st      *store.Store
	handler *apiHandlers
}

func newListFixture(t *testing.T, extraDeployments int) listFixture {
	t.Helper()
	st, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	node := func(name string) *store.Node {
		n, err := st.RegisterNode(name, store.HashToken(name), name)
		must(err)
		return n
	}
	a, b, gone := node("list-a"), node("list-b"), node("list-gone")
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/list", Revision: "main", Name: "org/list",
		Operations: `[{"type":"openai.chat","endpoint":"/v1/chat/completions"}]`})
	must(err)
	doomed, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/doomed", Revision: "main", Name: "org/doomed"})
	must(err)
	_, err = st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: a.ID, State: proto.MFDownloading, Path: "/m", SizeBytes: 100, Downloaded: 40, RateBPS: 7, Message: "shard 2/5"})
	must(err)
	_, err = st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: b.ID, State: proto.MFPresent, Path: "/m"})
	must(err)
	_, err = st.UpsertImage(&store.Image{Name: "list-image", ImageRef: "example/engine:1"})
	must(err)

	deployment := func(d store.Deployment) *store.Deployment {
		created, err := st.CreateDeployment(&d)
		must(err)
		return created
	}
	instance := func(dep *store.Deployment, nodeID uint, uuid string) {
		_, err := st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: nodeID, State: proto.InstRunning,
			Image: "example/engine:1", GPUIndexes: "[0]", Ports: "[8000]",
			DesiredStateJSON: `{"launch":{"command_mode":"engine","argv":["serve"],"env":{"K":"V"},"model_mount":"/model"}}`})
		must(err)
	}
	single := deployment(store.Deployment{Name: "single", ModelID: model.ID, NodeID: a.ID, Replicas: 1, QuickParams: `{"max_model_len":4096}`,
		CatalogSelectionSnapshot: `{"matched_by":"spec","acquisition":{"proxy":"http://user:pw@proxy:1"}}`})
	instance(single, a.ID, "inst-single")
	multi := deployment(store.Deployment{Name: "multi", ModelID: model.ID, NodeID: a.ID, NodeIDs: fmt.Sprintf("[%d,%d]", a.ID, b.ID), NNodes: 2})
	instance(multi, a.ID, "inst-multi-0")
	instance(multi, b.ID, "inst-multi-1")
	scheduledAway := deployment(store.Deployment{Name: "scheduled-away", ModelID: model.ID, NodeID: a.ID, Replicas: 1})
	instance(scheduledAway, b.ID, "inst-scheduled-away")
	_, err = st.CreateInstance(&store.Instance{UUID: "inst-scheduled-away-stopped", DeploymentID: scheduledAway.ID, NodeID: a.ID, State: proto.InstStopped})
	must(err)
	waiting := deployment(store.Deployment{Name: "waiting", ModelID: model.ID, NodeID: b.ID})
	st.AddDeploymentEventCode(waiting.ID, store.EventKindSchedule, "DEPLOYMENT_UNSCHEDULABLE", map[string]any{"reason": "no_gpu"})
	must(st.DB().Model(&store.InstanceEvent{}).Where("deployment_id = ?", waiting.ID).
		Update("created_at", time.Now().Add(-48*time.Hour)).Error)
	st.AddDeploymentEventCode(waiting.ID, store.EventKindSchedule, "DEPLOYMENT_UNSCHEDULABLE", map[string]any{"reason": "no_gpu"})
	deployment(store.Deployment{Name: "no-model-file", ModelID: doomed.ID, NodeID: b.ID})
	orphan := deployment(store.Deployment{Name: "node-gone", ModelID: model.ID, NodeID: gone.ID})
	instance(orphan, gone.ID, "inst-orphan")
	deployment(store.Deployment{Name: "model-gone", ModelID: doomed.ID, NodeID: a.ID})
	for i := 0; i < extraDeployments; i++ {
		extra := deployment(store.Deployment{Name: fmt.Sprintf("extra-%d", i), ModelID: model.ID, NodeID: a.ID})
		instance(extra, a.ID, fmt.Sprintf("inst-extra-%d", i))
	}
	must(st.DB().Delete(&store.Node{}, gone.ID).Error)
	must(st.DB().Delete(&store.Model{}, doomed.ID).Error)
	return listFixture{st: st, handler: &apiHandlers{d: Deps{Store: st}}}
}

func (f listFixture) get(t *testing.T, serve http.HandlerFunc, path string) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	serve(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

// The list reads its joins in bulk and a single object reads them one by one.
// Both must describe a deployment identically, byte for byte.
func TestDeploymentListMatchesTheSingleDeploymentPath(t *testing.T) {
	f := newListFixture(t, 0)
	var listed []json.RawMessage
	if err := unmarshalList(f.get(t, f.handler.listDeployments, "/api/v1/deployments"), &listed); err != nil {
		t.Fatal(err)
	}
	deps, err := f.st.ListDeployments()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != len(deps) || len(deps) != 7 {
		t.Fatalf("listed %d deployments, store has %d, fixture seeds 7", len(listed), len(deps))
	}
	states := map[string]string{}
	for i := range deps {
		single, err := json.Marshal(f.handler.deploymentDTO(&deps[i], false))
		if err != nil {
			t.Fatal(err)
		}
		if string(single) != string(listed[i]) {
			t.Errorf("deployment %q differs\n single: %s\n listed: %s", deps[i].Name, single, listed[i])
		}
		var row dto.Deployment
		if err := json.Unmarshal(listed[i], &row); err != nil {
			t.Fatal(err)
		}
		states[row.Name] = row.RuntimeState
		switch row.Name {
		case "multi":
			if len(row.NodeUUIDs) != 2 || len(row.ModelProgress) != 2 {
				t.Errorf("multi: node_uuids=%v model_progress=%d, want 2 and 2", row.NodeUUIDs, len(row.ModelProgress))
			}
			if len(row.RuntimeNodeNames) != 2 || row.RuntimeNodeNames[0] != "list-a" || row.RuntimeNodeNames[1] != "list-b" {
				t.Errorf("multi: runtime nodes=%v, want list-a and list-b", row.RuntimeNodeNames)
			}
		case "single":
			if row.ModelName != "org/list" || row.NodeName != "list-a" || len(row.Operations) != 1 || len(row.ModelProgress) != 1 || row.ModelProgress[0].Message != "shard 2/5" {
				t.Errorf("single: joined fields are wrong: %s", listed[i])
			}
		case "scheduled-away":
			if row.NodeName != "list-a" || len(row.RuntimeNodeNames) != 1 || row.RuntimeNodeNames[0] != "list-b" {
				t.Errorf("scheduled-away should preserve target node and report actual runtime node: %s", listed[i])
			}
		case "node-gone":
			if row.NodeUUID != "" || row.NodeName != "" {
				t.Errorf("node-gone still names a node: %s", listed[i])
			}
		case "model-gone":
			if row.ModelName != "" || len(row.Operations) != 0 {
				t.Errorf("model-gone still names a model: %s", listed[i])
			}
		}
	}
	// The fixture is only a fair comparison if the stalled branch is exercised.
	if states["waiting"] == states["no-model-file"] {
		t.Errorf("the stalled deployment derived the same runtime state %q as an unstalled one; the schedule-event join is not exercised", states["waiting"])
	}
}

func TestInstanceListMatchesTheSingleInstancePath(t *testing.T) {
	f := newListFixture(t, 0)
	var listed []json.RawMessage
	if err := unmarshalList(f.get(t, f.handler.listInstances, "/api/v1/instances"), &listed); err != nil {
		t.Fatal(err)
	}
	insts, err := f.st.ListInstances()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != len(insts) || len(insts) != 6 {
		t.Fatalf("listed %d instances, store has %d, fixture seeds 6", len(listed), len(insts))
	}
	for i := range insts {
		single, err := json.Marshal(f.handler.instanceDTO(&insts[i]))
		if err != nil {
			t.Fatal(err)
		}
		if string(single) != string(listed[i]) {
			t.Errorf("instance %q differs\n single: %s\n listed: %s", insts[i].UUID, single, listed[i])
		}
	}
	var first dto.Instance
	if err := json.Unmarshal(listed[len(listed)-1], &first); err != nil {
		t.Fatal(err)
	}
	if first.UUID != "inst-single" || first.NodeUUID != "list-a" || first.ModelProgress == nil || first.CommandMode != "engine" {
		t.Errorf("inst-single: joined fields are wrong: %s", listed[len(listed)-1])
	}
}

// A list that issues queries per row is what made 964 deployments take 1.8 s
// and hold the only database connection while doing it.
func TestListQueryCountDoesNotGrowWithTheFleet(t *testing.T) {
	count := func(extra int) (deployments, instances int64) {
		f := newListFixture(t, extra)
		var queries atomic.Int64
		tally := func(*gorm.DB) { queries.Add(1) }
		if err := f.st.DB().Callback().Query().Before("gorm:query").Register("test:count_query", tally); err != nil {
			t.Fatal(err)
		}
		if err := f.st.DB().Callback().Row().Before("gorm:row").Register("test:count_row", tally); err != nil {
			t.Fatal(err)
		}
		f.get(t, f.handler.listDeployments, "/api/v1/deployments")
		deployments = queries.Swap(0)
		f.get(t, f.handler.listInstances, "/api/v1/instances")
		return deployments, queries.Load()
	}
	smallDeps, smallInsts := count(0)
	largeDeps, largeInsts := count(200)
	if smallDeps == 0 || smallInsts == 0 {
		t.Fatalf("the query counter saw nothing (deployments=%d instances=%d)", smallDeps, smallInsts)
	}
	if largeDeps != smallDeps {
		t.Errorf("GET /deployments: %d queries for 7 rows, %d for 207", smallDeps, largeDeps)
	}
	if largeInsts != smallInsts {
		t.Errorf("GET /instances: %d queries for 4 rows, %d for 204", smallInsts, largeInsts)
	}
	// The joins are eleven bulk reads plus the deployments themselves and the
	// policy; the latest four are the node requirements, the newest
	// scheduling event, and the model files the instances read with their
	// storages, each one query for the whole list.
	// Preparation adds two node-scoped batch reads: storage targets and health.
	if smallDeps > 15 {
		t.Errorf("GET /deployments issues %d queries; the joins include two scoped storage reads", smallDeps)
	}
}
