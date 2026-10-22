package hub

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An agent that walks candidates itself gets the whole planned list, rules
// first, in one call; the hub does not walk them for it.
func TestPullImageSendsThePlannedCandidatesInOneCall(t *testing.T) {
	var mu sync.Mutex
	var pulls []proto.ImagePullParams
	h := newArchiveURLHub(t, "file:hub_pull_plan_candidates?mode=memory&cache=shared", func(env proto.Envelope) string {
		var req struct {
			Method string                `json:"method"`
			Params proto.ImagePullParams `json:"params"`
		}
		if json.Unmarshal(env.D, &req) == nil && req.Method == proto.RPCImagePull {
			mu.Lock()
			pulls = append(pulls, req.Params)
			mu.Unlock()
		}
		return `{"result":{"state":"pulling"}}`
	})
	h.hub.session(h.node.UUID).caps = []string{proto.CapImagePullCandidates}
	if _, err := h.store.CreatePullSourceRule(store.PullSourceRuleSpec{
		Match: "registry.example", Endpoints: []string{"harbor.corp.example/proxy"}, AllowUpstream: true,
		Requirements: nodelabel.Requirements{nodelabel.KeyNode: {h.node.UUID}},
	}); err != nil {
		t.Fatal(err)
	}
	row, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, imagecatalog.FetchRequest{})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(pulls) != 1 {
		t.Fatalf("pull calls = %d, want one", len(pulls))
	}
	want := []proto.ImagePullCandidate{
		{Ref: "harbor.corp.example/proxy/team/runner:v1"},
		{Ref: "quay.io/team/runner:v1", Rank: true},
		{Ref: "registry.example/team/runner:v1", Rank: true},
	}
	if !reflect.DeepEqual(pulls[0].Candidates, want) || pulls[0].EffectiveRef != want[0].Ref || pulls[0].TransferID != row.TransferID {
		t.Fatalf("pull params = %+v, want candidates %+v", pulls[0], want)
	}
	// The rule endpoint leads unranked: it is the one the node tries first.
	if row.Source != "harbor.corp.example" && !strings.Contains(row.Message, "harbor.corp.example/proxy/team/runner:v1") {
		t.Fatalf("row = source %q message %q, want the endpoint named", row.Source, row.Message)
	}
}

// A copy a deployment's template declared reaches the pre-pull too.
func TestPullImageUsesTheCallersCopies(t *testing.T) {
	var mu sync.Mutex
	var pulls []proto.ImagePullParams
	h := newArchiveURLHub(t, "file:hub_pull_plan_copies?mode=memory&cache=shared", func(env proto.Envelope) string {
		var req struct {
			Method string                `json:"method"`
			Params proto.ImagePullParams `json:"params"`
		}
		if json.Unmarshal(env.D, &req) == nil && req.Method == proto.RPCImagePull {
			mu.Lock()
			pulls = append(pulls, req.Params)
			mu.Unlock()
		}
		return `{"result":{"state":"pulling"}}`
	})
	h.hub.session(h.node.UUID).caps = []string{proto.CapImagePullCandidates}
	copies := []string{"ghcr.io/team/runner:v1", "registry.example/team/runner:v1"}
	if _, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, imagecatalog.FetchRequest{Copies: copies}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(pulls) != 1 || len(pulls[0].Candidates) != 2 || pulls[0].Candidates[0].Ref != "ghcr.io/team/runner:v1" {
		t.Fatalf("pulls = %+v", pulls)
	}
	// Both copies are ranked: the node picks between them, so the row names
	// neither until it reports.
	if row, err := h.store.ImageNode(h.image.ID, h.node.ID); err != nil || row.Source != "" || row.Message != "pulling: the node tries 2 sources, nearest first" {
		t.Fatalf("row = %+v err=%v", row, err)
	}
}

// A replay plans each instance's candidates from the rules as they are now. An
// instance stored by an older server (a flat pull order, an explicit choice and
// a proxy, no copies) gets its copies from that order and the catalog, and
// loses the deployment-scoped choice. The same rules, read once, plan the
// node's model downloads and its proxy; a download they forbid stays out of
// the replay.
func TestReplayPlansInstanceCandidatesFromTheCurrentRules(t *testing.T) {
	h := newArchiveURLHub(t, "file:hub_pull_plan_replay?mode=memory&cache=shared", func(proto.Envelope) string { return `{"result":{}}` })
	for _, spec := range []store.PullSourceRuleSpec{
		{Match: "quay.io", Endpoints: []string{"quay.mirror.example"}, AllowUpstream: false},
		{Match: "https://huggingface.co", Endpoints: []string{"https://hf-mirror.com"}, AllowUpstream: true},
		{Match: "https://modelscope.cn", AllowUpstream: false},
	} {
		if _, err := h.store.CreatePullSourceRule(spec); err != nil {
			t.Fatal(err)
		}
	}
	instances := []proto.InstanceDesiredState{
		{
			UUID: "legacy", DesiredState: "running", Image: h.image.ImageRef,
			ImagePullRefs: []string{"registry.example/team/runner:v1"}, ImagePullExplicit: true, ImagePullProxy: "http://p:3128",
		},
		{UUID: "gone", DesiredState: "stopped", Image: h.image.ImageRef},
	}
	ds := proto.DesiredState{Instances: instances, ModelFiles: []proto.ModelFileTask{
		{Key: "hf", Source: "huggingface", SourceID: "org/a", Acquisition: proto.ModelAcquireDownload},
		{Key: "ms", Source: "modelscope", SourceID: "org/b", Acquisition: proto.ModelAcquireDownload},
		{Key: "peer", Source: "modelscope", SourceID: "org/c", Acquisition: proto.ModelAcquirePeer},
	}}
	if _, err := h.store.CreatePullProxy(store.PullProxySpec{HTTPSProxy: "http://idc-proxy:3128", NoProxy: "files.corp"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.hub.planReplay(h.node, &ds); err != nil {
		t.Fatal(err)
	}
	if ds.Network.HTTPSProxy != "http://idc-proxy:3128" || ds.Network.NoProxy != "files.corp" {
		t.Fatalf("network = %+v, want the node's proxy entry", ds.Network)
	}
	if len(ds.ModelFiles) != 2 || ds.ModelFiles[0].Key != "hf" || ds.ModelFiles[1].Key != "peer" {
		t.Fatalf("model files = %+v, want the forbidden download left out", ds.ModelFiles)
	}
	if want := []proto.ModelFetchCandidate{{Base: "https://hf-mirror.com"}, {Base: "https://huggingface.co", Upstream: true}}; !reflect.DeepEqual(ds.ModelFiles[0].Candidates, want) || ds.ModelFiles[0].Endpoint != "https://hf-mirror.com" {
		t.Fatalf("download = %+v, want candidates %+v", ds.ModelFiles[0], want)
	}
	legacy := instances[0]
	if !reflect.DeepEqual(legacy.ImageSources, []string{"registry.example/team/runner:v1", "quay.io/team/runner:v1"}) {
		t.Fatalf("sources = %v", legacy.ImageSources)
	}
	want := []proto.ImagePullCandidate{
		{Ref: "quay.mirror.example/team/runner:v1"},
		{Ref: "registry.example/team/runner:v1", Rank: true},
	}
	if !reflect.DeepEqual(legacy.ImagePullCandidates, want) {
		t.Fatalf("candidates = %+v, want %+v", legacy.ImagePullCandidates, want)
	}
	if legacy.ImagePullRef != want[0].Ref || len(legacy.ImagePullRefs) != 2 || legacy.ImagePullExplicit || legacy.ImagePullProxy != "" {
		t.Fatalf("legacy fields = ref %q refs %v explicit %v proxy %q", legacy.ImagePullRef, legacy.ImagePullRefs, legacy.ImagePullExplicit, legacy.ImagePullProxy)
	}
	if len(instances[1].ImagePullCandidates) != 0 {
		t.Fatal("a stopped instance was planned")
	}
}
