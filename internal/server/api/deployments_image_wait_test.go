package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deployment"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDeploymentHTTPWaitsForCurrentImageTransferPastScheduleTimeout(t *testing.T) {
	rig := newTokenRig(t, func(d *Deps) { d.Lists = deployment.NewLists(d.Store, nil, nil) })
	t.Cleanup(func() { _ = rig.st.Close() })
	node, err := rig.st.RegisterNode("image-target", store.HashToken("image-target"), "image-target")
	if err != nil {
		t.Fatal(err)
	}
	model, err := rig.st.UpsertModel(&store.Model{Name: "weights", Source: "local", SourceID: "weights"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/weights"}); err != nil {
		t.Fatal(err)
	}
	image, err := rig.st.UpsertImage(&store.Image{Name: "runner", ImageRef: "fixture/runner:v1", Engine: "vllm", Source: "community", Enabled: true, RegistrySources: `["mirror.fixture/runner:v1"]`})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := rig.st.CreateDeployment(&store.Deployment{Name: "image-wait", State: store.DeployActive, Replicas: 1, NodeID: node.ID, ModelID: model.ID, Image: "mirror.fixture/runner:v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.st.DB().Create(&store.InstanceEvent{DeploymentID: dep.ID, Kind: store.EventKindSchedule, Code: "DEPLOYMENT_UNSCHEDULABLE", Data: `{"scheduler":{"code":"image_delivery_waiting","params":{"node":"image-target","image":"fixture/runner:v1"}}}`, CreatedAt: time.Now().Add(-11 * time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	transfer, err := rig.st.UpsertImageNode(&store.ImageNode{ImageID: image.ID, NodeID: node.ID, State: store.ImageStateSyncing, Source: "peer", TransferID: "same-transfer", SizeBytes: 27 << 30, TransferredBytes: 247 * (1 << 30) / 10, RateBPS: 59 << 20})
	if err != nil {
		t.Fatal(err)
	}
	check := func(want string) {
		t.Helper()
		for _, path := range []string{"/api/v1/deployments?page=1", "/api/v1/deployments?page=1&state=" + want, fmt.Sprintf("/api/v1/deployments/%d", dep.ID)} {
			rec := rig.do(rig.session, http.MethodGet, path, "")
			if rec.Code != 200 {
				t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
			}
			var got dto.Deployment
			if strings.HasPrefix(path, "/api/v1/deployments?") {
				var page dto.DeploymentPage
				if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
					t.Fatal(err)
				}
				if len(page.Items) != 1 {
					t.Fatalf("page=%s", rec.Body)
				}
				got.State = page.Items[0].State
				got.RuntimeState = page.Items[0].RuntimeState
			} else if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			runtimeWant := want
			if want == "deleting" {
				runtimeWant = "stopped"
			}
			if got.State != want || got.RuntimeState != runtimeWant {
				t.Fatalf("%s state/runtime=%s/%s, want %s; body=%s", path, got.State, got.RuntimeState, want, rec.Body)
			}
		}
	}
	check("pending")
	same, err := rig.st.ImageNode(image.ID, node.ID)
	if err != nil || same.ID != transfer.ID || same.TransferID != "same-transfer" || same.TransferredBytes != transfer.TransferredBytes || same.State != store.ImageStateSyncing {
		t.Fatalf("reads interrupted transfer: %+v %v", same, err)
	}
	// A historical image wait alone must not suppress a real stall.
	transfer.State = store.ImageStateFailed
	transfer.Message = "peer transfer failed"
	if _, err := rig.st.UpsertImageNode(transfer); err != nil {
		t.Fatal(err)
	}
	check("failed")
	// The same image on another node and another image on this node are
	// unrelated to the deployment's preparation; neither extends its budget.
	otherNode, err := rig.st.RegisterNode("image-other", store.HashToken("image-other"), "image-other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.st.UpsertImageNode(&store.ImageNode{ImageID: image.ID, NodeID: otherNode.ID, State: store.ImageStateSyncing, TransferID: "wrong-node"}); err != nil {
		t.Fatal(err)
	}
	otherImage, err := rig.st.UpsertImage(&store.Image{Name: "other", ImageRef: "fixture/other:v1", Engine: "vllm", Source: "community", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.st.UpsertImageNode(&store.ImageNode{ImageID: otherImage.ID, NodeID: node.ID, State: store.ImageStatePulling, TransferID: "wrong-image"}); err != nil {
		t.Fatal(err)
	}
	check("failed")
	transfer.State = store.ImageStateSyncing
	if _, err := rig.st.UpsertImageNode(transfer); err != nil {
		t.Fatal(err)
	}
	check("pending")
	// Both the cached list and the lazy detail must surface an unreadable
	// ledger, without turning unknown transfer facts into failed deployment.
	if err := rig.st.DB().Exec("ALTER TABLE image_nodes RENAME TO unreadable_image_nodes").Error; err != nil {
		t.Fatal(err)
	}
	rig.st.Bus.PublishChange("image", image.ImageRef, "node")
	for _, path := range []string{"/api/v1/deployments?page=1", fmt.Sprintf("/api/v1/deployments/%d", dep.ID)} {
		rec := rig.do(rig.session, http.MethodGet, path, "")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("unreadable image ledger %s: %d %s", path, rec.Code, rec.Body)
		}
	}
	if err := rig.st.DB().Exec("ALTER TABLE unreadable_image_nodes RENAME TO image_nodes").Error; err != nil {
		t.Fatal(err)
	}
	rig.st.Bus.PublishChange("image", image.ImageRef, "node")
	check("pending")
	// A registry pull is preparation just like a peer copy.
	transfer.State = store.ImageStatePulling
	if _, err := rig.st.UpsertImageNode(transfer); err != nil {
		t.Fatal(err)
	}
	check("pending")
	// An actual placement refusal remains a failure even while an image is
	// also being copied in the background.
	rig.st.AddDeploymentEventCode(dep.ID, store.EventKindSchedule, store.EventDeploymentUnschedulable, map[string]any{"scheduler": map[string]any{"code": "no_free_gpu", "params": map[string]any{"node": node.UUID}}})
	check("failed")
	rig.st.AddDeploymentEventCode(dep.ID, store.EventKindSchedule, store.EventDeploymentUnschedulable, map[string]any{"scheduler": map[string]any{"code": "image_delivery_waiting", "params": map[string]any{"image": image.ImageRef}}})
	check("pending")
	if changed, err := rig.st.CancelImageNodeTransfer(image.ID, node.ID, "cancelled by user"); err != nil || !changed {
		t.Fatalf("cancel=%v %v", changed, err)
	}
	check("failed")
	transfer.State = store.ImageStateSyncing
	if _, err := rig.st.UpsertImageNode(transfer); err != nil {
		t.Fatal(err)
	}
	check("pending")
	// Completion must clear a shared pending view; the controller can then
	// start the replica, and the public view follows its actual state.
	transfer.State = store.ImageStatePresent
	if _, err := rig.st.UpsertImageNode(transfer); err != nil {
		t.Fatal(err)
	}
	check("failed")
	// A stopped deployment with no instances needs no image fact.
	if err := rig.st.DB().Exec("ALTER TABLE image_nodes RENAME TO unreadable_image_nodes").Error; err != nil {
		t.Fatal(err)
	}
	rig.st.Bus.PublishChange("image", image.ImageRef, "node")
	if err := rig.st.SetDeploymentState(dep.ID, store.DeployStopped); err != nil {
		t.Fatal(err)
	}
	check("stopped")
	if err := rig.st.DB().Exec("ALTER TABLE unreadable_image_nodes RENAME TO image_nodes").Error; err != nil {
		t.Fatal(err)
	}
	rig.st.Bus.PublishChange("image", image.ImageRef, "node")
	if err := rig.st.SetDeploymentState(dep.ID, store.DeployActive); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.st.CreateInstance(&store.Instance{UUID: "image-ready", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstStarting}); err != nil {
		t.Fatal(err)
	}
	check("starting")
	if err := rig.st.SetNodeState(node.UUID, store.NodeStateOnline); err != nil {
		t.Fatal(err)
	}
	if err := rig.st.SetInstanceState("image-ready", proto.InstRunning, ""); err != nil {
		t.Fatal(err)
	}
	check("running")
	// A running attempt or stopped/deleting desired state does not depend on
	// an image wait from its history. An unrelated broken image ledger must
	// not make those already-known states unreadable.
	if err := rig.st.DB().Exec("ALTER TABLE image_nodes RENAME TO unreadable_image_nodes").Error; err != nil {
		t.Fatal(err)
	}
	rig.st.Bus.PublishChange("image", image.ImageRef, "node")
	check("running")
	if err := rig.st.SetDeploymentState(dep.ID, store.DeployStopped); err != nil {
		t.Fatal(err)
	}
	check("stopped")
	if err := rig.st.DeleteInstance("image-ready"); err != nil {
		t.Fatal(err)
	}
	if err := rig.st.SetDeploymentState(dep.ID, store.DeployDeleting); err != nil {
		t.Fatal(err)
	}
	check("deleting")
}
