package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// directCopyFailed leaves target's row as the hub leaves it when holder could
// not connect to target and the copy was not relayed.
func directCopyFailed(t *testing.T, st *store.Store, image *store.Image, target, holder *store.Node) {
	t.Helper()
	if _, err := st.UpsertImageNode(&store.ImageNode{
		ImageID: image.ID, ImageVersionID: image.ID, NodeID: target.ID, State: store.ImageStateFailed, Source: "peer", SourceNodeID: holder.ID,
		TransferErrorCode: messages.ErrorPeerDirectConnectFailed,
		Message:           "send peer image archive: direct peer connection failed: dial 10.9.0.1:34787: i/o timeout",
	}); err != nil {
		t.Fatal(err)
	}
}

// replicaPullFailed reports, as the node's agent does, that the replica the
// controller launched could not pull its image from any source.
func replicaPullFailed(t *testing.T, st *store.Store, dep *store.Deployment, target *store.Node) {
	t.Helper()
	instances := launched(t, st, dep)
	if len(instances) != 1 {
		t.Fatalf("instances = %d, want the one replica that pulls", len(instances))
	}
	if err := st.UpdateInstanceStatusFromNode(target.ID, proto.InstanceStatus{
		UUID: instances[0].UUID, State: proto.InstFailed, Phase: proto.PhaseImagePull,
		Message:        "pull gpustack/runner:v1: every source failed",
		RuntimeFailure: &proto.RuntimeFailure{Code: proto.ErrorImagePullFailed, Phase: proto.PhaseImagePull, Detail: "quay.io/gpustack/runner:v1: dial tcp: i/o timeout"},
	}); err != nil {
		t.Fatal(err)
	}
}

// afterRetryBackoff moves the controller's clock past the wait between a
// replica's failed attempt and its next one.
func afterRetryBackoff(c *Controller) {
	at := c.now().Add(c.retryBackoff + time.Second)
	c.now = func() time.Time { return at }
}

func reconcile(t *testing.T, c *Controller, dep *store.Deployment) {
	t.Helper()
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
}

// The order is direct copy, then the node's own pull, then the relay: a node
// no holder connects to pulls first, and only when that pull has failed is
// the image fed to it through the management server.
func TestRelayIsTheLastResortAfterTheDirectCopyAndThePullFailed(t *testing.T) {
	c, st, dep, target, holder, image, hub := imageCopyFixture(t, "controller_image_relay_last")
	directCopyFailed(t, st, image, target, holder)
	reconcile(t, c, dep)
	if len(hub.copies) != 0 || len(launched(t, st, dep)) != 1 {
		t.Fatalf("copies=%+v instances=%d: after a failed direct copy the replica pulls, nothing is relayed yet", hub.copies, len(launched(t, st, dep)))
	}
	replicaPullFailed(t, st, dep, target)
	afterRetryBackoff(c)
	reconcile(t, c, dep)
	if len(hub.copies) != 1 || !hub.copies[0].relay || hub.copies[0].source != holder.UUID || hub.copies[0].target != target.UUID {
		t.Fatalf("copies = %+v, want one relayed copy from the holder after the pull failed", hub.copies)
	}
	if got := len(launched(t, st, dep)); got != 1 {
		t.Fatalf("instances = %d: another replica started while the relayed copy runs", got)
	}
	// The relayed copy arrives: the replica starts on the image.
	if _, err := st.UpsertImageNode(&store.ImageNode{ImageID: image.ID, ImageVersionID: image.ID, NodeID: target.ID, State: store.ImageStatePresent, Source: "peer", SourceNodeID: holder.ID}); err != nil {
		t.Fatal(err)
	}
	reconcile(t, c, dep)
	if len(hub.copies) != 1 {
		t.Fatalf("copies = %+v: an image that arrived was copied again", hub.copies)
	}
}

// The relay is one path whichever holder feeds it: after it failed for a
// target, the next holder is not tried through it; the replica pulls again
// and its failure is what the deployment shows.
func TestAFailedRelayIsNotRetriedThroughTheNextHolder(t *testing.T) {
	c, st, dep, target, holder, image, hub := imageCopyFixture(t, "controller_image_relay_once")
	second := anotherHolder(t, st, hub, image, "controller_image_relay_once-holder-2", "10.9.0.3")
	directCopyFailed(t, st, image, target, holder)
	reconcile(t, c, dep)
	replicaPullFailed(t, st, dep, target)
	afterRetryBackoff(c)
	reconcile(t, c, dep)
	if len(hub.copies) != 1 || !hub.copies[0].relay {
		t.Fatalf("copies = %+v, want the one relayed copy", hub.copies)
	}
	relayedFrom, err := st.NodeByUUID(hub.copies[0].source)
	if err != nil {
		t.Fatal(err)
	}
	// The relay gave up: no progress within its limit. The hub fails the
	// row of that transfer.
	running, err := st.ImageNode(image.ID, target.ID)
	if err != nil || running.TransferID == "" || running.SourceNodeID != relayedFrom.ID {
		t.Fatalf("row of the relayed copy = %+v, err = %v", running, err)
	}
	if err := st.FailImageSyncIfActive(image.ID, target.ID, running.TransferID, "relay peer connection: no_progress_timeout"); err != nil {
		t.Fatal(err)
	}
	if failed, err := st.ImageNode(image.ID, target.ID); err != nil || failed.State != store.ImageStateFailed {
		t.Fatalf("row after the relay gave up = %+v, err = %v", failed, err)
	}
	reconcile(t, c, dep)
	for _, copy := range hub.copies[1:] {
		if copy.relay {
			t.Fatalf("copies = %+v: the relay was tried again for the same target (holders %s, %s)", hub.copies, holder.UUID, second.UUID)
		}
	}
	if !c.peerCool.cooling(relayPeers, target.ID, c.now()) {
		t.Fatal("the relay is not cooling for the target")
	}
	if !c.peerCool.cooling(relayPeers, target.ID, c.now().Add(peerCooldown-time.Minute)) {
		t.Fatal("the relay's cooldown ended before three hours from its failure")
	}
	if c.peerCool.cooling(relayPeers, target.ID, c.now().Add(peerCooldown+time.Minute)) {
		t.Fatal("the relay is still ruled out after the cooldown")
	}
}

// A node whose pull did not fail is never fed through the relay, however
// many holders could not connect to it.
func TestNoRelayWhileThePullHasNotFailed(t *testing.T) {
	c, st, dep, target, holder, image, hub := imageCopyFixture(t, "controller_image_relay_needs_pull_failure")
	directCopyFailed(t, st, image, target, holder)
	reconcile(t, c, dep)
	reconcile(t, c, dep)
	if len(hub.copies) != 0 {
		t.Fatalf("copies = %+v, want none: the replica is pulling", hub.copies)
	}
	if c.peerCool.cooling(relayPeers, target.ID, c.now()) {
		t.Fatal("the relay was ruled out without having been tried")
	}
}

// The pull the controller makes ahead of a multi-node launch records its
// failure on the image's row; that failure opens the relay as a replica's
// own does.
func TestAFailedPrePullOpensTheRelay(t *testing.T) {
	c, st, dep, target, holder, image, hub := imageCopyFixture(t, "controller_image_relay_prepull")
	c.peerCool.cool(holder.ID, target.ID, c.now())
	c.peerCool.cool(managementPeers, target.ID, c.now())
	if _, err := st.UpsertImageNode(&store.ImageNode{
		ImageID: image.ID, ImageVersionID: image.ID, NodeID: target.ID, State: store.ImageStateFailed, Source: "quay.io/gpustack/runner:v1",
		Message: "pull quay.io/gpustack/runner:v1: dial tcp: i/o timeout",
	}); err != nil {
		t.Fatal(err)
	}
	reconcile(t, c, dep)
	if len(hub.copies) != 1 || !hub.copies[0].relay {
		t.Fatalf("copies = %+v, want one relayed copy after the recorded pull failure", hub.copies)
	}
	if got := len(launched(t, st, dep)); got != 0 {
		t.Fatalf("instances = %d: the replica started while the relayed copy runs", got)
	}
}

// A pull that failed long ago says nothing about now: the node pulls again
// before anything is relayed.
func TestAnOldPullFailureDoesNotOpenTheRelay(t *testing.T) {
	c, st, dep, target, holder, image, hub := imageCopyFixture(t, "controller_image_relay_old_failure")
	c.peerCool.cool(holder.ID, target.ID, c.now())
	if _, err := st.UpsertImageNode(&store.ImageNode{
		ImageID: image.ID, ImageVersionID: image.ID, NodeID: target.ID, State: store.ImageStateFailed, Source: "quay.io/gpustack/runner:v1",
		Message: "pull quay.io/gpustack/runner:v1: dial tcp: i/o timeout",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.ImageNode{}).Where("image_version_id = ? AND node_id = ?", image.ID, target.ID).
		UpdateColumn("updated_at", c.now().Add(-peerCooldown-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	reconcile(t, c, dep)
	if len(hub.copies) != 0 || len(launched(t, st, dep)) != 1 {
		t.Fatalf("copies=%+v instances=%d, want no copy and the replica pulling again", hub.copies, len(launched(t, st, dep)))
	}
}

// A relayed copy that could not be started rules nothing out: the replica
// pulls again and the next launch asks the relay once more.
func TestARelayThatDidNotStartIsNotCounted(t *testing.T) {
	c, st, dep, target, holder, image, hub := imageCopyFixture(t, "controller_image_relay_not_started")
	directCopyFailed(t, st, image, target, holder)
	reconcile(t, c, dep)
	replicaPullFailed(t, st, dep, target)
	afterRetryBackoff(c)
	// As the hub answers when the holder went away between the pick and the start.
	hub.copyErr = errors.New("selected source node is offline or no longer has this image")
	reconcile(t, c, dep)
	if len(hub.copies) != 1 || !hub.copies[0].relay {
		t.Fatalf("copies = %+v, want the one relay request", hub.copies)
	}
	if c.peerCool.cooling(relayPeers, target.ID, c.now()) {
		t.Fatal("a relay that never started was counted against the target")
	}
	if got := len(launched(t, st, dep)); got != 2 {
		t.Fatalf("instances = %d, want the failed attempt and a new one pulling", got)
	}
}

// An attempt that failed to pull another image, before the deployment was
// edited, does not open the relay for the image it runs now.
func TestAPullFailureOfAnotherImageDoesNotOpenTheRelay(t *testing.T) {
	c, st, dep, target, holder, image, hub := imageCopyFixture(t, "controller_image_relay_other_image")
	directCopyFailed(t, st, image, target, holder)
	reconcile(t, c, dep)
	replicaPullFailed(t, st, dep, target)
	if err := st.DB().Model(&store.Instance{}).Where("deployment_id = ?", dep.ID).UpdateColumn("image", "gpustack/runner:v0").Error; err != nil {
		t.Fatal(err)
	}
	afterRetryBackoff(c)
	reconcile(t, c, dep)
	if len(hub.copies) != 0 {
		t.Fatalf("copies = %+v, want none: this image's pull has not failed", hub.copies)
	}
}

// A failure another transfer left on the row is not the relay's: an operator
// started a copy by hand after the relayed one, and that one failed.
func TestAnotherTransfersFailureIsNotCountedAgainstTheRelay(t *testing.T) {
	c, st, dep, target, holder, image, hub := imageCopyFixture(t, "controller_image_relay_other_transfer")
	directCopyFailed(t, st, image, target, holder)
	reconcile(t, c, dep)
	replicaPullFailed(t, st, dep, target)
	afterRetryBackoff(c)
	reconcile(t, c, dep)
	if len(hub.copies) != 1 || !hub.copies[0].relay {
		t.Fatalf("copies = %+v, want the one relayed copy", hub.copies)
	}
	if _, err := st.UpsertImageNode(&store.ImageNode{
		ImageID: image.ID, ImageVersionID: image.ID, NodeID: target.ID, State: store.ImageStateFailed, Source: "peer", SourceNodeID: holder.ID,
		LastTransferID: "started-by-hand", Message: "docker load: unexpected EOF",
	}); err != nil {
		t.Fatal(err)
	}
	reconcile(t, c, dep)
	if c.peerCool.cooling(relayPeers, target.ID, c.now()) {
		t.Fatal("a copy started by hand was counted as the relay's failure")
	}
	if !c.peerCool.cooling(holder.ID, target.ID, c.now()) {
		t.Fatal("the pair of the failed copy is not cooling")
	}
}
