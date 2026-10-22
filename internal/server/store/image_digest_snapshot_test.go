package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestApplyImageNodeStatusFillsInstanceImageDigestSnapshot(t *testing.T) {
	st, err := Open("file:store_image_digest_snapshot?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	image, err := st.UpsertImage(&Image{ImageRef: "example/rolling:latest", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("node-digest", HashToken("digest-token"), "digest")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImageNode(&ImageNode{ImageID: image.ID, NodeID: node.ID, State: ImageStatePulling}); err != nil {
		t.Fatal(err)
	}
	instance, err := st.CreateInstance(&Instance{UUID: "rolling-instance", NodeID: node.ID, Image: image.ImageRef, State: proto.InstPending})
	if err != nil {
		t.Fatal(err)
	}
	// A failed attempt never ran the image that arrives later.
	failed, err := st.CreateInstance(&Instance{UUID: "failed-attempt", NodeID: node.ID, Image: image.ImageRef, State: proto.InstFailed})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyImageNodeStatus(image.ID, node.ID, proto.ImageNodeStatus{State: ImageStatePresent, ImageID: "sha256:first"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.InstanceByUUID(instance.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ImageDigestID != "sha256:first" {
		t.Fatalf("image digest snapshot = %q", got.ImageDigestID)
	}
	if got, _ := st.InstanceByUUID(failed.UUID); got.ImageDigestID != "" {
		t.Fatalf("failed attempt took the image digest %q", got.ImageDigestID)
	}
	if err := st.ApplyImageNodeStatus(image.ID, node.ID, proto.ImageNodeStatus{State: ImageStatePresent, ImageID: "sha256:later"}); err != nil {
		t.Fatal(err)
	}
	got, err = st.InstanceByUUID(instance.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ImageDigestID != "sha256:first" {
		t.Fatalf("existing image digest snapshot was rewritten: %q", got.ImageDigestID)
	}
}
