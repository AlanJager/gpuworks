package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// componentRig is the save-to rig plus a template component (a text encoder
// the template fetches beside the primary).
func componentRig(t *testing.T) (*saveToRig, templateComponent) {
	t.Helper()
	r := newSaveToRig(t)
	model, err := r.st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "org/base", Revision: "abc", ArtifactSubfolder: "text_encoder"})
	if err != nil {
		t.Fatal(err)
	}
	binding := deploy.ComponentBinding{Name: "text-encoder", Role: "auxiliary", StoreModelID: model.ID, WeightBytes: 66_726_510_529}
	return r, templateComponent{binding: binding, model: model}
}

func (r *saveToRig) localHolds(t *testing.T, model *store.Model, node *store.Node) {
	t.Helper()
	if _, err := r.st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: r.st.DefaultLocalStorageID(), NodeID: node.ID, Path: store.SharedModelRelPath(model), State: proto.MFPresent, SizeBytes: 10}); err != nil {
		t.Fatal(err)
	}
}

func (r *saveToRig) nfsHolds(t *testing.T, model *store.Model) {
	t.Helper()
	if _, err := r.st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: r.nfs.ID, Path: store.SharedModelRelPath(model), State: proto.MFPresent, SizeBytes: 10}); err != nil {
		t.Fatal(err)
	}
}

func (r *saveToRig) plan(t *testing.T, source *dto.ModelSource, component templateComponent, pinned []uint, nodes ...*store.Node) componentPlan {
	t.Helper()
	if len(nodes) == 0 {
		nodes = []*store.Node{r.node}
	}
	saveTo, err := r.h.resolveSaveToStorage(source, r.model, nodes)
	if err != nil {
		t.Fatal(err)
	}
	plans, err := r.h.planComponents(source, r.model, []templateComponent{component}, saveTo, pinned, nodes)
	if err != nil || len(plans) != 1 {
		t.Fatalf("planComponents = %+v, %v", plans, err)
	}
	return plans[0]
}

// How a template component reaches the target nodes, by where the primary and
// the component already are. MiniMax-H3-Image on the H20 server was the
// "primary on the NFS, component nowhere" case: the text encoder went into
// every node's local storage. With no storage named it now follows the
// primary into that NFS, written once; every other case keeps the default
// order of store.ModelSourceOn.
func TestPlanComponentsByWhereTheModelsAre(t *testing.T) {
	type want struct {
		plan    string
		storage bool
		follows bool
		ready   int
	}
	cases := []struct {
		name     string
		setup    func(t *testing.T, r *saveToRig, c templateComponent)
		source   *dto.ModelSource
		pinned   func(r *saveToRig) []uint
		want     want
		writable bool
	}{
		{name: "nothing anywhere, no storage named", writable: true,
			setup: func(*testing.T, *saveToRig, templateComponent) {},
			want:  want{plan: componentPlanNodeDownload}},
		{name: "primary on the NFS, component nowhere", writable: true,
			setup: func(t *testing.T, r *saveToRig, _ templateComponent) { r.nfsHoldsModel(t) },
			want:  want{plan: componentPlanSharedDownload, storage: true, follows: true}},
		{name: "primary on a read-only NFS", writable: false,
			setup: func(t *testing.T, r *saveToRig, _ templateComponent) { r.nfsHoldsModel(t) },
			want:  want{plan: componentPlanNodeDownload}},
		{name: "both on the NFS", writable: true,
			setup: func(t *testing.T, r *saveToRig, c templateComponent) { r.nfsHoldsModel(t); r.nfsHolds(t, c.model) },
			want:  want{plan: componentPlanReady, ready: 1}},
		{name: "only the primary on the node", writable: true,
			setup: func(t *testing.T, r *saveToRig, _ templateComponent) { r.localHolds(t, r.model, r.node) },
			want:  want{plan: componentPlanNodeDownload}},
		{name: "only the component on the node", writable: true,
			setup: func(t *testing.T, r *saveToRig, c templateComponent) { r.localHolds(t, c.model, r.node) },
			want:  want{plan: componentPlanReady, ready: 1}},
		{name: "both on the node", writable: true,
			setup: func(t *testing.T, r *saveToRig, c templateComponent) {
				r.localHolds(t, r.model, r.node)
				r.localHolds(t, c.model, r.node)
			},
			want: want{plan: componentPlanReady, ready: 1}},
		{name: "a local storage named", writable: true,
			setup:  func(t *testing.T, r *saveToRig, _ templateComponent) { r.nfsHoldsModel(t) },
			source: &dto.ModelSource{StorageID: 0}, // replaced below with the default local storage
			want:   want{plan: componentPlanNodeDownload}},
		{name: "pinned to local storage", writable: true,
			setup:  func(t *testing.T, r *saveToRig, _ templateComponent) { r.nfsHoldsModel(t) },
			pinned: func(r *saveToRig) []uint { return []uint{r.st.DefaultLocalStorageID()} },
			want:   want{plan: componentPlanNodeDownload}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, component := componentRig(t)
			r.nfsState(t, proto.StorageStateReady, tc.writable)
			tc.setup(t, r, component)
			source := tc.source
			if source != nil && source.StorageID == 0 {
				source = &dto.ModelSource{StorageID: r.st.DefaultLocalStorageID()}
			}
			var pinned []uint
			if tc.pinned != nil {
				pinned = tc.pinned(r)
			}
			got := r.plan(t, source, component, pinned)
			if got.plan != tc.want.plan || (got.storage != nil) != tc.want.storage || got.followsPrimary != tc.want.follows || got.readyNodes != tc.want.ready || got.nodes != 1 {
				t.Fatalf("plan = %s storage=%v follows=%v ready=%d/%d; want %+v", got.plan, got.storage, got.followsPrimary, got.readyNodes, got.nodes, tc.want)
			}
			if got.storage != nil && got.storage.ID != r.nfs.ID {
				t.Fatalf("storage = %s, want %s", got.storage.Name, r.nfs.Name)
			}
		})
	}
}

func TestPlanComponentsWithASharedStorageNamed(t *testing.T) {
	r, component := componentRig(t)
	r.nfsState(t, proto.StorageStateReady, true)
	got := r.plan(t, &dto.ModelSource{StorageID: r.nfs.ID}, component, nil)
	if got.plan != componentPlanSharedDownload || got.storage == nil || got.storage.ID != r.nfs.ID || got.followsPrimary {
		t.Fatalf("plan = %+v, want the named NFS, not following the primary", got)
	}
}

// One node of two already holds the component: the create still fetches it
// once into the NFS for the other, and the forecast says how many are ready.
func TestPlanComponentsCountsReadyNodes(t *testing.T) {
	r, component := componentRig(t)
	second, err := r.st.RegisterNode("gpu-b", store.HashToken("b"), "gpu-b")
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range []*store.Node{r.node, second} {
		if err := r.st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{
			{ID: r.nfs.ID, State: proto.StorageStateReady, Path: r.nfs.Path, Writable: true},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	r.nfsHoldsModel(t)
	r.localHolds(t, component.model, r.node)
	got := r.plan(t, nil, component, nil, r.node, second)
	if got.plan != componentPlanSharedDownload || !got.followsPrimary || got.readyNodes != 1 || got.nodes != 2 {
		t.Fatalf("plan = %s follows=%v ready=%d/%d", got.plan, got.followsPrimary, got.readyNodes, got.nodes)
	}
}

// A write of the component into the NFS that someone else started is not one
// this deployment's nodes wait for, so the create does not claim it: each
// node fetches the component itself, and the forecast says so.
func TestPlanComponentsLeavesAWriteSomeoneElseStarted(t *testing.T) {
	r, component := componentRig(t)
	r.nfsState(t, proto.StorageStateReady, true)
	r.nfsHoldsModel(t)
	if _, err := r.st.UpsertModelFile(&store.ModelFile{ModelID: component.model.ID, StorageID: r.nfs.ID, Path: store.SharedModelRelPath(component.model), State: proto.MFDownloading, WriterNodeID: r.node.ID, DownloadDeploymentID: 99}); err != nil {
		t.Fatal(err)
	}
	if got := r.plan(t, nil, component, nil); got.plan != componentPlanNodeDownload || got.storage != nil {
		t.Fatalf("plan = %+v, want node_download", got)
	}
}

// The create acts on the plan: one write owned by the deployment, through a
// writer node, recorded on the deployment's timeline; the deployment's nodes
// wait for it, another deployment's do not.
func TestStartComponentDownloadsWritesOnceAndSaysSo(t *testing.T) {
	r, component := componentRig(t)
	r.nfsState(t, proto.StorageStateReady, true)
	r.nfsHoldsModel(t)
	r.h.d.NodeSync = nodeSyncCallbacks{full: func(string) error { return nil }}
	dep, err := r.st.CreateDeploymentWithRequirements(&store.Deployment{Name: "h3-image", ModelID: r.model.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := r.plan(t, nil, component, nil)
	if status, err := r.h.startComponentDownloads([]componentPlan{plan}, []*store.Node{r.node}, dep.ID); err != nil {
		t.Fatalf("startComponentDownloads = %d, %v", status, err)
	}
	row, err := r.st.ModelFileAt(store.FileRef{ModelID: component.model.ID, StorageID: r.nfs.ID})
	if err != nil || row.State != proto.MFPending || row.WriterNodeID != r.node.ID || row.DownloadDeploymentID != dep.ID {
		t.Fatalf("component row = %+v, %v", row, err)
	}
	events, err := r.st.DeploymentEvents(dep.ID, 10)
	if err != nil || len(events) != 1 || events[0].Code != store.EventDeploymentComponentDownload {
		t.Fatalf("events = %+v, %v", events, err)
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(events[0].Data), &params); err != nil {
		t.Fatal(err)
	}
	if params["component"] != "text-encoder" || params["storage"] != r.nfs.Name || params["node"] != r.node.DisplayName() ||
		params["bytes"] != float64(66_726_510_529) || params["follows_primary"] != true || !strings.Contains(params["model"].(string), "org/base") {
		t.Fatalf("event params = %v", params)
	}
	src, err := r.st.ModelSourceOn(component.model.ID, r.node, store.SourceRules{Deployment: dep.ID})
	if err != nil || src.Action != store.SourceWaitShared || src.Location.Storage.ID != r.nfs.ID {
		t.Fatalf("own deployment source = %+v, %v; want to wait for %s", src, err, r.nfs.Name)
	}
	if src, err := r.st.ModelSourceOn(component.model.ID, r.node, store.SourceRules{Deployment: dep.ID + 1}); err != nil || src.Action != store.SourceAcquire {
		t.Fatalf("another deployment's source = %+v, %v; want to acquire", src, err)
	}
	// Nothing more to write: a second call starts nothing and records nothing.
	if _, err := r.h.startComponentDownloads([]componentPlan{{templateComponent: component, plan: componentPlanReady}}, []*store.Node{r.node}, dep.ID); err != nil {
		t.Fatal(err)
	}
	if events, _ := r.st.DeploymentEvents(dep.ID, 10); len(events) != 1 {
		t.Fatalf("a ready component recorded %d events", len(events))
	}
}

// The forecast's wire form: plan, counts, storage and the size the template
// declares.
func TestAcquisitionComponentsWire(t *testing.T) {
	r, component := componentRig(t)
	encoded, err := json.Marshal(acquisitionComponents([]componentPlan{
		{templateComponent: component, plan: componentPlanSharedDownload, nodes: 2, readyNodes: 1, storage: r.nfs, followsPrimary: true},
		{templateComponent: component, plan: componentPlanReady, nodes: 2, readyNodes: 2},
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"name":"text-encoder","role":"auxiliary","model":"` + component.model.DisplayName() + `","size_bytes":66726510529,"plan":"shared_download","nodes":2,"ready_nodes":1,"storage_id":` + jsonUint(r.nfs.ID) + `,"storage":"team-nfs","follows_primary":true},` +
		`{"name":"text-encoder","role":"auxiliary","model":"` + component.model.DisplayName() + `","size_bytes":66726510529,"plan":"ready","nodes":2,"ready_nodes":2}]`
	if string(encoded) != want {
		t.Fatalf("wire =\n%s\nwant\n%s", encoded, want)
	}
}

func TestTemplateComponentsSkipsThePrimary(t *testing.T) {
	r, component := componentRig(t)
	got, err := r.h.templateComponents([]deploy.ComponentBinding{
		{Name: "image", Role: "primary", StoreModelID: r.model.ID},
		component.binding,
		{Name: "unresolved", Role: "auxiliary"},
	})
	if err != nil || len(got) != 1 || got[0].model.ID != component.model.ID || got[0].binding.Name != "text-encoder" {
		t.Fatalf("templateComponents = %+v, %v", got, err)
	}
	if _, err := r.h.templateComponents([]deploy.ComponentBinding{{Name: "gone", Role: "auxiliary", StoreModelID: 9999}}); err == nil {
		t.Fatal("a component whose model row is gone must fail the create")
	}
}

// The forecast through the evaluate handler, for a manual placement of a
// template with a draft component: first nowhere, then with the primary on
// an NFS the node reads (the component follows it there), then with the
// component already on the node.
func TestEvaluateForecastsTemplateComponents(t *testing.T) {
	st := newAcquisitionStore(t)
	fixture := deploymentFlatFixture()
	fixture.ModelID = "acme/target"
	fixture.Specs[0].WeightSources = `[{"provider":"huggingface","repo_id":"Acme/Target","revision":"target-rev"}]`
	fixture.Specs[0].Components = `[
		{"name":"target","role":"primary","weights":{"quantization":"fp8","sources":[{"provider":"huggingface","repo_id":"Acme/Target","revision":"target-rev"}]},"weight_bytes":17179869184,"mount_path":"/models/target"},
		{"name":"draft","role":"speculative_draft","model_id":"acme/draft","weights":{"quantization":"bf16","sources":[{"provider":"huggingface","repo_id":"Acme/Draft","revision":"draft-rev"}]},"weight_bytes":2147483648,"mount_path":"/models/draft"}
	]`
	draft := store.InferenceModel{ModelID: "acme/draft", Source: "upstream", Architecture: "draft", Specs: []store.InferenceSpecRow{{
		Name: "bf16", Engine: "vllm", Image: "image", Quantization: "bf16", WeightBytes: 2 << 30,
		WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Draft","revision":"draft-rev"}]`, GPUs: `{"vendor":"nvidia","cc":">=8.0"}`,
	}}}
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture, draft}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("gpu-h", store.HashToken("h"), "gpu-h")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: "GPU-h-0", Name: "NVIDIA H100", Vendor: "nvidia",
		ComputeCapability: "9.0", ArchID: "sm_90", MemoryTotal: 80 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st, Online: func(string) bool { return true }}}
	evaluate := func() []dto.AcquisitionComponent {
		t.Helper()
		body := fmt.Sprintf(`{"template_id":%d,"template_spec_source":"upstream","template_spec_name":"fp8-balanced","node_uuid":%q,"quick_params":{},"placement":{}}`, template.ID, node.UUID)
		rec := httptest.NewRecorder()
		h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Acquisition *struct {
				Components []dto.AcquisitionComponent `json:"components"`
			} `json:"acquisition"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Acquisition == nil {
			t.Fatalf("acquisition missing: %v %s", err, rec.Body.String())
		}
		return resp.Acquisition.Components
	}
	modelBySource := func(repo string) *store.Model {
		t.Helper()
		models, err := st.ListModels()
		if err != nil {
			t.Fatal(err)
		}
		for i := range models {
			if models[i].SourceID == repo {
				return &models[i]
			}
		}
		t.Fatalf("no model row for %s in %+v", repo, models)
		return nil
	}

	got := evaluate()
	if len(got) != 1 || got[0].Name != "draft" || got[0].Role != "speculative_draft" || got[0].SizeBytes != 2<<30 ||
		got[0].Plan != componentPlanNodeDownload || got[0].Nodes != 1 || got[0].ReadyNodes != 0 || got[0].Storage != "" {
		t.Fatalf("nothing anywhere: components = %+v", got)
	}

	nfs, err := st.CreateStorage(&store.Storage{Name: "models-nfs", Type: proto.StorageNFS, Enabled: true, AllNodes: true,
		Path: "/mnt/gpuworks/models-nfs", Export: "nfs.example.internal:/export/models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{
		{ID: nfs.ID, State: proto.StorageStateReady, Path: nfs.Path, Writable: true},
	}}); err != nil {
		t.Fatal(err)
	}
	primary := modelBySource("Acme/Target")
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: primary.ID, StorageID: nfs.ID, Path: store.SharedModelRelPath(primary), State: proto.MFPresent, SizeBytes: 10}); err != nil {
		t.Fatal(err)
	}
	if got := evaluate(); len(got) != 1 || got[0].Plan != componentPlanSharedDownload || !got[0].FollowsPrimary || got[0].Storage != "models-nfs" || got[0].StorageID != nfs.ID {
		t.Fatalf("primary on the NFS: components = %+v", got)
	}

	component := modelBySource("Acme/Draft")
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: component.ID, StorageID: st.DefaultLocalStorageID(), NodeID: node.ID, Path: store.SharedModelRelPath(component), State: proto.MFPresent, SizeBytes: 10}); err != nil {
		t.Fatal(err)
	}
	if got := evaluate(); len(got) != 1 || got[0].Plan != componentPlanReady || got[0].ReadyNodes != 1 {
		t.Fatalf("component on the node: components = %+v", got)
	}
}
