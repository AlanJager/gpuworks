package store

import (
	"encoding/json"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestUpdateInstanceObservationIsNodeBoundAndDoesNotChangeLifecycle(t *testing.T) {
	st, err := Open("file:instance-runtime-observation?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	nodeA := Node{UUID: "node-a", Name: "a"}
	nodeB := Node{UUID: "node-b", Name: "b"}
	if err := st.DB().Create(&nodeA).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&nodeB).Error; err != nil {
		t.Fatal(err)
	}
	instance, err := st.CreateInstance(&Instance{
		UUID: "instance-observed", NodeID: nodeA.ID, State: proto.InstRunning, ContainerID: "lifecycle-container-value",
	})
	if err != nil {
		t.Fatal(err)
	}
	report := proto.InstanceStatus{UUID: instance.UUID, RuntimeObservation: &proto.InstanceRuntimeObservation{
		Runtime: "docker",
		Container: proto.RuntimeContainerObservation{
			Status: proto.ObservationObserved, ContainerID: "immutable-actual-id", ImageID: "sha256:old-image",
			Entrypoint: []string{}, Cmd: []string{}, Args: []string{}, Env: map[string]string{}, Mounts: []proto.RuntimeMountObservation{}, Devices: []string{},
		},
		Image: proto.RuntimeImageObservation{
			Status: proto.ObservationObserved, ImageID: "sha256:old-image", Entrypoint: []string{}, Cmd: []string{}, Env: map[string]string{}, Volumes: []string{},
		},
		Process: proto.RuntimeProcessObservation{Status: proto.ObservationUnknown, Reason: "not_collected"},
	}}
	if err := st.UpdateInstanceStatusFromNode(nodeB.ID, report); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong-node update error=%v", err)
	}
	unchanged, _ := st.InstanceByUUID(instance.UUID)
	if unchanged.RuntimeObservation != "" {
		t.Fatal("wrong-node report was persisted")
	}
	if err := st.UpdateInstanceStatusFromNode(nodeA.ID, report); err != nil {
		t.Fatal(err)
	}
	got, _ := st.InstanceByUUID(instance.UUID)
	if got.State != proto.InstRunning || got.ContainerID != "lifecycle-container-value" {
		t.Fatalf("observation changed lifecycle fields: state=%q container=%q", got.State, got.ContainerID)
	}
	if got.RuntimeObservationReceivedAt == nil || got.RuntimeObservationReceivedAt.IsZero() {
		t.Fatal("server receive time was not recorded")
	}
	var observation proto.InstanceRuntimeObservation
	if err := json.Unmarshal([]byte(got.RuntimeObservation), &observation); err != nil {
		t.Fatal(err)
	}
	if observation.Container.ContainerID != "immutable-actual-id" || observation.Image.ImageID != "sha256:old-image" {
		t.Fatalf("observation=%+v", observation)
	}
}

// The image a replica runs is the one its container was created from, as the
// agent observes it, for an image the catalog knows or not; an observation
// that did not see the container leaves the recorded one alone.
func TestRuntimeObservationRecordsTheImageTheReplicaRuns(t *testing.T) {
	st, err := Open("file:instance-runtime-image?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	node := Node{UUID: "node-img", Name: "img"}
	if err := st.DB().Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	instance, err := st.CreateInstance(&Instance{UUID: "replica-img", NodeID: node.ID, State: proto.InstRunning, Image: "vllm/vllm-openai:v1", ImageDigestID: "sha256:from-ledger"})
	if err != nil {
		t.Fatal(err)
	}
	observe := func(imageID string) {
		t.Helper()
		report := proto.InstanceStatus{UUID: instance.UUID, State: proto.InstRunning, RuntimeObservation: &proto.InstanceRuntimeObservation{
			Runtime:   "docker",
			Container: proto.RuntimeContainerObservation{Status: proto.ObservationObserved, ImageID: imageID},
		}}
		if err := st.UpdateInstanceStatusFromNode(node.ID, report); err != nil {
			t.Fatal(err)
		}
	}
	observe("sha256:container-image")
	got, _ := st.InstanceByUUID(instance.UUID)
	if got.ImageDigestID != "sha256:container-image" {
		t.Fatalf("image digest = %q, want the container's", got.ImageDigestID)
	}
	observe("")
	got, _ = st.InstanceByUUID(instance.UUID)
	if got.ImageDigestID != "sha256:container-image" {
		t.Fatalf("an observation without the container cleared the image: %q", got.ImageDigestID)
	}
	// nerdctl reports the image's name where docker reports its ID; a name
	// is what every replica shares, so it is not recorded as one.
	observe("docker.io/vllm/vllm-openai:v1")
	got, _ = st.InstanceByUUID(instance.UUID)
	if got.ImageDigestID != "sha256:container-image" {
		t.Fatalf("an image name was taken for an image ID: %q", got.ImageDigestID)
	}
}
