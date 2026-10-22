package controller

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// copyTestHub is the agent hub as the automatic image copy sees it: it
// records each copy the controller starts and marks the target's ledger row
// syncing, as the real hub does, or refuses the copy when told to.
type copyTestHub struct {
	*distributedTestHub
	st      *store.Store
	copies  []imageCopyCall
	copyErr error
}

type imageCopyCall struct {
	image          uint
	target, source string
	rateKiB        int64
}

func (h *copyTestHub) SyncImageFromRate(_ context.Context, imageID uint, target, source string, rate int64) (*store.ImageNode, error) {
	h.copies = append(h.copies, imageCopyCall{imageID, target, source, rate})
	if h.copyErr != nil {
		return nil, h.copyErr
	}
	node, err := h.st.NodeByUUID(target)
	if err != nil {
		return nil, err
	}
	src, err := h.st.NodeByUUID(source)
	if err != nil {
		return nil, err
	}
	return h.st.UpsertImageNode(&store.ImageNode{ImageID: imageID, ImageVersionID: imageID, NodeID: node.ID, State: store.ImageStateSyncing, Source: "peer", SourceNodeID: src.ID})
}

// imageCopyFixture is a launchable deployment on target whose catalog image
// target lacks and holder has. Both nodes are online.
func imageCopyFixture(t *testing.T, name string) (*Controller, *store.Store, *store.Deployment, *store.Node, *store.Node, *store.Image, *copyTestHub) {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	target, err := st.RegisterNode(name+"-target", store.HashToken(name+"t"), name+"-target")
	if err != nil {
		t.Fatal(err)
	}
	holder, err := st.RegisterNode(name+"-holder", store.HashToken(name+"h"), name+"-holder")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{
		NodeID: target.ID, Index: 0, UUID: name + "-gpu-0",
		Name: "NVIDIA A100-SXM4-80GB", Vendor: "nvidia", ArchID: "sm_80",
		ComputeCapability: "8.0", MemoryTotal: 80 << 30,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if target, err = st.NodeWithGPUs(target.UUID); err != nil {
		t.Fatal(err)
	}
	image, err := st.UpsertImage(&store.Image{
		Name: "runner", ImageRef: "gpustack/runner:v1", Source: "official", Engine: "vllm", Enabled: true,
		RegistrySources: `["quay.io/gpustack/runner:v1"]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImageNode(&store.ImageNode{ImageID: image.ID, ImageVersionID: image.ID, NodeID: holder.ID, State: store.ImageStatePresent}); err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "fixture/" + name, Revision: "main", Name: name,
		WeightBytes: 1 << 30,
		Meta:        `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: target.ID, State: proto.MFPresent, Path: "/models/" + name}); err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{
		Name: name, ModelID: model.ID, NodeID: target.ID, ServedName: name, Engine: "vllm", State: "active",
		Image: image.ImageRef, Placement: `{"tp":1,"gpus":[0]}`, QuickParams: `{"max_model_len":512}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := &copyTestHub{
		distributedTestHub: &distributedTestHub{online: map[string]bool{target.UUID: true, holder.UUID: true}, updates: map[string][]proto.StateUpdate{}},
		st:                 st,
	}
	return New(st, hub, nil), st, dep, target, holder, image, hub
}

func launched(t *testing.T, st *store.Store, dep *store.Deployment) []store.Instance {
	t.Helper()
	instances, err := st.InstancesForDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	return instances
}

// A replica on a node that lacks the image waits while a node of its sync
// group copies it over, and starts once it has arrived.
func TestReplicaWaitsForACopyFromASyncGroupPeer(t *testing.T) {
	c, st, dep, target, holder, image, hub := imageCopyFixture(t, "controller_image_copy_waits")
	ctx := context.Background()
	if err := c.reconcileDeployment(ctx, dep.ID); err != nil {
		t.Fatal(err)
	}
	if len(launched(t, st, dep)) != 0 {
		t.Fatal("the replica started before the copy arrived")
	}
	if len(hub.copies) != 1 || hub.copies[0] != (imageCopyCall{image.ID, target.UUID, holder.UUID, 0}) {
		t.Fatalf("copies = %+v, want one from the holder", hub.copies)
	}
	if waiting := c.imageWaits.waiting(image.ID); len(waiting) != 1 || waiting[0] != dep.ID {
		t.Fatalf("waiting deployments = %v", waiting)
	}
	// A copy in flight is waited on, not started again.
	if err := c.reconcileDeployment(ctx, dep.ID); err != nil {
		t.Fatal(err)
	}
	if len(hub.copies) != 1 || len(launched(t, st, dep)) != 0 {
		t.Fatalf("copies = %+v while one was running", hub.copies)
	}
	if _, err := st.UpsertImageNode(&store.ImageNode{ImageID: image.ID, ImageVersionID: image.ID, NodeID: target.ID, State: store.ImageStatePresent, Source: "peer"}); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(ctx, dep.ID); err != nil {
		t.Fatal(err)
	}
	instances := launched(t, st, dep)
	if len(instances) != 1 {
		t.Fatalf("instances = %+v, want the replica started", instances)
	}
	if len(c.imageWaits.waiting(image.ID)) != 0 {
		t.Fatal("a started deployment is still remembered as waiting")
	}
	updates := hub.updates[target.UUID]
	if len(updates) != 1 || len(updates[0].Instances) != 1 {
		t.Fatalf("updates = %+v", updates)
	}
	spec := updates[0].Instances[0]
	if len(spec.ImageSources) != 2 || len(spec.ImagePullCandidates) != 2 || spec.ImagePullExplicit || spec.ImagePullProxy != "" {
		t.Fatalf("pushed spec image facts = sources %v candidates %+v explicit %v proxy %q", spec.ImageSources, spec.ImagePullCandidates, spec.ImagePullExplicit, spec.ImagePullProxy)
	}
}

// A copy that failed is not waited on: the replica pulls the image itself,
// and that pair of nodes is not tried again for a while. This is the case of
// two nodes that both reach the server but not each other (a node on an
// internal network and one on a public cloud).
func TestFailedCopyFallsBackToThePullAndCoolsThePair(t *testing.T) {
	c, st, dep, target, holder, image, hub := imageCopyFixture(t, "controller_image_copy_failed")
	if _, err := st.UpsertImageNode(&store.ImageNode{
		ImageID: image.ID, ImageVersionID: image.ID, NodeID: target.ID, State: store.ImageStateFailed, Source: "peer", SourceNodeID: holder.ID,
		Message: "send peer image archive: dial model sync target: 10.42.14.163:34787: connect: no route to host",
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if len(hub.copies) != 0 {
		t.Fatalf("the pair that just failed was tried again: %+v", hub.copies)
	}
	if len(launched(t, st, dep)) != 1 {
		t.Fatal("the replica did not start to pull the image itself")
	}
	if !c.peerCool.cooling(holder.ID, target.ID, c.now()) {
		t.Fatal("the failed pair is not cooling")
	}
	if _, found, _ := st.LatestScheduleEvent(dep.ID); found {
		t.Fatal("a fallback that launched recorded a refusal")
	}
}

func TestCopyThatCannotStartFallsBackAtOnce(t *testing.T) {
	c, st, dep, target, holder, _, hub := imageCopyFixture(t, "controller_image_copy_refused")
	hub.copyErr = errors.New("source node cannot connect to target direct address")
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if len(hub.copies) != 1 || len(launched(t, st, dep)) != 1 {
		t.Fatalf("copies=%+v instances=%d, want one refused copy and the replica started", hub.copies, len(launched(t, st, dep)))
	}
	if !c.peerCool.cooling(holder.ID, target.ID, c.now()) {
		t.Fatal("a refused pair is not cooling")
	}
}

// Nodes that share no sync group never copy from each other: the replica
// pulls the image without waiting.
func TestNoSharedSyncGroupMeansNoCopy(t *testing.T) {
	c, st, dep, _, holder, _, hub := imageCopyFixture(t, "controller_image_copy_no_group")
	groups, _ := st.SyncGroups()
	if err := st.DeleteSyncGroup(groups[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetNodeLabels(holder.UUID, map[string]string{"site": "idc-b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSyncGroup(store.SyncGroupSpec{Name: "B", Requirements: nodelabel.Requirements{"site": {"idc-b"}}}); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if len(hub.copies) != 0 || len(launched(t, st, dep)) != 1 {
		t.Fatalf("copies=%+v instances=%d, want no copy and the replica started", hub.copies, len(launched(t, st, dep)))
	}
}

// A group's rate cap is the copy's.
func TestCopyRunsAtTheSharedGroupsCap(t *testing.T) {
	c, st, dep, _, _, _, hub := imageCopyFixture(t, "controller_image_copy_rate")
	groups, _ := st.SyncGroups()
	if _, err := st.UpdateSyncGroup(groups[0].ID, store.SyncGroupSpec{Name: groups[0].Name, RateLimitKiB: 2048}); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if len(hub.copies) != 1 || hub.copies[0].rateKiB != 2048 {
		t.Fatalf("copies = %+v, want the group's cap", hub.copies)
	}
}

// A copy finishing re-queues the deployments waiting for it, and only those.
func TestImageEventRequeuesOnlyWaitingDeployments(t *testing.T) {
	c, _, dep, _, _, image, _ := imageCopyFixture(t, "controller_image_copy_event")
	c.imageWaits.set(dep.ID, image.ID)
	c.imageWaits.set(dep.ID+100, image.ID+100)
	c.onEvent(bus.Event{Topic: "image", Key: strconv.FormatUint(uint64(image.ID), 10), Reason: "node"})
	c.q.mu.Lock()
	_, waiting := c.q.pending[depKey(dep.ID)]
	_, other := c.q.pending[depKey(dep.ID+100)]
	c.q.mu.Unlock()
	if !waiting || other {
		t.Fatal("the image event did not re-queue exactly the deployment waiting for that image")
	}
}

// A ledger row records what it recorded: the reason omits source and detail
// rather than carrying empty ones, because the client renders params by name and
// an empty value would render as an empty promise.
func TestImageDeliveryFailedOmitsParamsTheLedgerDidNotRecord(t *testing.T) {
	reason := scheduler.ImageDeliveryFailed("node-a", "gpustack/runner:v1", "  ", "")
	if reason.Code != scheduler.ReasonImageDeliveryFailed {
		t.Fatalf("code = %q", reason.Code)
	}
	if reason.Params["node"] != "node-a" || reason.Params["image"] != "gpustack/runner:v1" {
		t.Fatalf("params = %+v, want the node and image always named", reason.Params)
	}
	if _, ok := reason.Params["source"]; ok {
		t.Fatalf("a blank source was carried: %+v", reason.Params)
	}
	if _, ok := reason.Params["detail"]; ok {
		t.Fatalf("an empty detail was carried: %+v", reason.Params)
	}
}

// Rules that leave a node nothing to pull from refuse the replica with a
// reason the deployment shows; nothing is launched to fail later.
func TestNoAllowedPullSourceRefusesTheReplica(t *testing.T) {
	c, st, dep, _, _, _, _ := imageCopyFixture(t, "controller_image_source_forbidden")
	groups, _ := st.SyncGroups()
	if err := st.DeleteSyncGroup(groups[0].ID); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"docker.io", "quay.io"} {
		if _, err := st.CreatePullSourceRule(store.PullSourceRuleSpec{Match: host, AllowUpstream: false}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if len(launched(t, st, dep)) != 0 {
		t.Fatal("a replica with no allowed source was launched")
	}
	ev, found, err := st.LatestScheduleEvent(dep.ID)
	if err != nil || !found || !strings.Contains(ev.Data, scheduler.ReasonImageSourceForbidden) {
		t.Fatalf("refusal = %+v found=%v err=%v, want %s", ev, found, err, scheduler.ReasonImageSourceForbidden)
	}
}
