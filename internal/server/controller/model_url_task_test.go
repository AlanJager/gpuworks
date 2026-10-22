package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/sourceurl"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func modelTaskStore(t *testing.T, name string) (*store.Store, *store.Node, *modelSyncHub, *Controller) {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode(name+"-node", store.HashToken("token"), name+"-node")
	if err != nil {
		t.Fatal(err)
	}
	hub := &modelSyncHub{online: map[string]bool{node.UUID: true}}
	return st, node, hub, New(st, hub, nil)
}

// A download task for a directory model carries the directory itself. It is
// the repository, not an accelerated route to one: no site rule moves it,
// even one that matches its host.
func TestPushModelFileSendsTheDirectoryAsTheTaskEndpoint(t *testing.T) {
	st, node, hub, c := modelTaskStore(t, "controller_url_task")
	directory, err := st.UpsertModel(&store.Model{
		Source: "url", SourceID: "Qwen/Qwen3-0.6B", SourceURL: "https://hf-mirror.example.com/Qwen/Qwen3-0.6B", Name: "Qwen3-0.6B",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePullSourceRule(store.PullSourceRuleSpec{Match: "https://hf-mirror.example.com", Endpoints: []string{"https://hf-mirror.other.example.com"}, AllowUpstream: false}); err != nil {
		t.Fatal(err)
	}
	file, err := st.PrepareModelDownload(store.FileRef{ModelID: directory.ID, NodeID: node.ID}, "url/Qwen_Qwen3-0.6B-main")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.pushModelFile(node, file, directory); err != nil {
		t.Fatal(err)
	}
	task := lastModelFileTask(t, hub)
	if task.Source != "url" || task.SourceID != "Qwen/Qwen3-0.6B" || task.Revision != "" {
		t.Fatalf("task identity = %+v", task)
	}
	if task.Endpoint != directory.SourceURL || !reflect.DeepEqual(task.Candidates, []proto.ModelFetchCandidate{{Base: directory.SourceURL, Upstream: true}}) {
		t.Fatalf("task = endpoint %q candidates %+v, want only the model's directory %q", task.Endpoint, task.Candidates, directory.SourceURL)
	}
}

// A repository download is sent with the addresses the node's site rules
// give, the same ones a replay would; one the rules forbid is not sent.
func TestPushModelFilePlansRepositoryDownloadsFromTheRules(t *testing.T) {
	st, node, hub, c := modelTaskStore(t, "controller_repo_task")
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-0.6B", Revision: "main", Name: "Qwen3-0.6B"})
	if err != nil {
		t.Fatal(err)
	}
	file, err := st.PrepareModelDownload(store.FileRef{ModelID: model.ID, NodeID: node.ID}, "huggingface/Qwen_Qwen3-0.6B-main")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.pushModelFile(node, file, model); err != nil {
		t.Fatal(err)
	}
	if task := lastModelFileTask(t, hub); task.Endpoint != "https://huggingface.co" ||
		!reflect.DeepEqual(task.Candidates, []proto.ModelFetchCandidate{{Base: "https://huggingface.co", Upstream: true}}) {
		t.Fatalf("direct repository task = %+v", task)
	}

	mirror, err := st.CreatePullSourceRule(store.PullSourceRuleSpec{Match: "https://huggingface.co", Endpoints: []string{"https://hf-mirror.example.com"}, AllowUpstream: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.pushModelFile(node, file, model); err != nil {
		t.Fatal(err)
	}
	want := []proto.ModelFetchCandidate{{Base: "https://hf-mirror.example.com"}, {Base: "https://huggingface.co", Upstream: true}}
	if task := lastModelFileTask(t, hub); task.Endpoint != "https://hf-mirror.example.com" || !reflect.DeepEqual(task.Candidates, want) {
		t.Fatalf("mirrored repository task = %+v", task)
	}

	if err := st.DeletePullSourceRule(mirror.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePullSourceRule(store.PullSourceRuleSpec{Match: "https://huggingface.co", AllowUpstream: false}); err != nil {
		t.Fatal(err)
	}
	pushed := len(hub.updates)
	if err := c.pushModelFile(node, file, model); !errors.Is(err, sourceurl.ErrNoAllowedSource) {
		t.Fatalf("forbidden push err = %v", err)
	}
	if len(hub.updates) != pushed {
		t.Fatal("a forbidden download was sent")
	}
}

// A deployment whose model the node can only download, from addresses the
// rules forbid, is refused with a reason it shows, and nothing is sent; once
// the rules allow it, the download starts.
func TestAForbiddenDownloadRefusesTheReplicaUntilTheRulesAllowIt(t *testing.T) {
	c, st, dep, target, _, _, hub := imageCopyFixture(t, "controller_weights_forbidden")
	if err := st.DB().Where("model_id = ?", dep.ModelID).Delete(&store.ModelFile{}).Error; err != nil {
		t.Fatal(err)
	}
	forbid, err := st.CreatePullSourceRule(store.PullSourceRuleSpec{Match: "https://huggingface.co", AllowUpstream: false})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	ev, found, err := st.LatestScheduleEvent(dep.ID)
	if err != nil || !found || !strings.Contains(ev.Data, scheduler.ReasonWeightsFetchForbidden) {
		t.Fatalf("refusal = %+v found=%v err=%v, want %s", ev, found, err, scheduler.ReasonWeightsFetchForbidden)
	}
	for _, update := range hub.updates[target.UUID] {
		for _, task := range update.ModelFiles {
			if task.Acquisition == proto.ModelAcquireDownload {
				t.Fatalf("a forbidden download was sent: %+v", task)
			}
		}
	}

	if err := st.DeletePullSourceRule(forbid.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	var sent *proto.ModelFileTask
	for _, update := range hub.updates[target.UUID] {
		for i := range update.ModelFiles {
			if update.ModelFiles[i].Acquisition == proto.ModelAcquireDownload {
				sent = &update.ModelFiles[i]
			}
		}
	}
	if sent == nil || len(sent.Candidates) != 1 || sent.Candidates[0].Base != "https://huggingface.co" {
		t.Fatalf("download after the rule was removed = %+v", sent)
	}
}

func lastModelFileTask(t *testing.T, hub *modelSyncHub) proto.ModelFileTask {
	t.Helper()
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for index := len(hub.updates) - 1; index >= 0; index-- {
		if len(hub.updates[index].ModelFiles) > 0 {
			return hub.updates[index].ModelFiles[0]
		}
	}
	t.Fatal("no model file task was pushed")
	return proto.ModelFileTask{}
}
