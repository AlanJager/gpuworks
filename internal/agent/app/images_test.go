package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	agentmodelsync "gpuworks.ai/gpuworks/internal/agent/modelsync"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// testImages is image work on driver d, with what it reported and logged.
func testImages(d runtime.Driver) (images *nodeImages, reported func() []proto.ImageNodeStatus, logged func() string) {
	var mu sync.Mutex
	var statuses []proto.ImageNodeStatus
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	images = &nodeImages{}
	images.wire(imageDeps{
		log:    func() *slog.Logger { return log },
		driver: func() runtime.Driver { return d },
		report: func(st proto.ImageNodeStatus) {
			mu.Lock()
			statuses = append(statuses, st)
			mu.Unlock()
		},
		stateDir: func() string { return "" },
	})
	return images, func() []proto.ImageNodeStatus {
			mu.Lock()
			defer mu.Unlock()
			return append([]proto.ImageNodeStatus(nil), statuses...)
		}, func() string {
			return logs.String()
		}
}

// The node answers an image RPC from the image table and still refuses a
// method nobody serves.
func TestOnRPCServesImageRPCsFromTheImageTable(t *testing.T) {
	n := &NodeRuntime{log: slog.Default()}
	got, err := n.OnRPC(context.Background(), proto.RPCImageCancel, json.RawMessage(`{"transfer_id":"gone"}`))
	if err != nil {
		t.Fatalf("image.cancel through the node: %v", err)
	}
	if result, ok := got.(proto.ImageCancelResult); !ok || result.State != "not_running" {
		t.Fatalf("image.cancel through the node = %+v, want not_running", got)
	}
	if _, err := n.OnRPC(context.Background(), "image.nonexistent", nil); err == nil {
		t.Fatal("the node accepted an unknown image method")
	}
}

// An operation that finds the slot taken says so once, then runs when the
// slot is free; one that finds it free says nothing.
func TestAQueuedImageOperationReportsThatItWaits(t *testing.T) {
	images, reported, _ := testImages(&presenceDriver{})
	images.lockSlot("repo/first:tag", "pulling")
	if got := reported(); len(got) != 0 {
		t.Fatalf("an operation that took a free slot reported %+v", got)
	}
	entered := make(chan struct{})
	go func() {
		images.lockSlot("repo/second:tag", "syncing")
		close(entered)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(reported()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the queued operation never reported that it waits")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-entered:
		t.Fatal("the queued operation ran while the slot was held")
	default:
	}
	images.slot.Unlock()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the queued operation did not run once the slot was free")
	}
	images.slot.Unlock()
	got := reported()
	if len(got) != 1 || got[0].ImageRef != "repo/second:tag" || got[0].State != "syncing" || got[0].Message == "" {
		t.Fatalf("waiting reports = %+v, want one for the queued image in its own state", got)
	}
}

// A peer relay attaches to a live transfer's listener once. A second relay,
// an unknown transfer and a transfer that already reported its end get
// nothing, and detaching closes what a waiter was given.
func TestImageRelayAttachesALiveTransferOnce(t *testing.T) {
	images, _, _ := testImages(&presenceDriver{})
	receiver := &agentmodelsync.Receiver{}
	images.syncs["repo/live:tag"] = &activeImageSync{transferID: "live", imageRef: "repo/live:tag", receiver: receiver}
	images.syncs["repo/done:tag"] = &activeImageSync{transferID: "done", imageRef: "repo/done:tag", receiver: receiver, reported: true}
	images.syncs["repo/starting:tag"] = &activeImageSync{transferID: "starting", imageRef: "repo/starting:tag"}

	for _, id := range []string{"unknown", "done", "starting", ""} {
		if got, release := images.attachRelay(id); got != nil || release != nil {
			t.Errorf("attachRelay(%q) handed out a listener", id)
		}
	}
	if images.relayAttached("live") != nil {
		t.Fatal("a transfer nothing attached to reports an attachment")
	}
	got, release := images.attachRelay("live")
	if got != receiver || release == nil {
		t.Fatalf("attachRelay(live) = %p, want the transfer's listener %p and a release", got, receiver)
	}
	if second, _ := images.attachRelay("live"); second != nil {
		t.Fatal("a second relay attached to the same transfer")
	}
	attached := images.relayAttached("live")
	if attached == nil {
		t.Fatal("an attached transfer gives a waiter nothing to wait on")
	}
	select {
	case <-attached:
		t.Fatal("the attachment ended before the relay detached")
	default:
	}
	release()
	select {
	case <-attached:
	case <-time.After(5 * time.Second):
		t.Fatal("detaching did not release the waiter")
	}
	if images.relayAttached("live") != nil {
		t.Fatal("a detached transfer still reports an attachment")
	}
	if again, _ := images.attachRelay("live"); again != receiver {
		t.Fatal("a detached transfer cannot be attached again")
	}
}

// A relayed image send or probe the server did not allow to be relayed is
// refused before the connection is used.
func TestImageRelayRefusesAnUnauthorizedTransfer(t *testing.T) {
	images, _, _ := testImages(&presenceDriver{})
	params := json.RawMessage(`{"transfer_id":"t","image_ref":"repo/image:tag","token":"secret","fingerprint":"f","endpoints":["192.0.2.1:1"]}`)
	if _, err := images.relaySend(context.Background(), params, nil); err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("relaySend without allow_relay: %v", err)
	}
	if _, err := images.relayProbe(context.Background(), params, nil); err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("relayProbe without allow_relay: %v", err)
	}
	for _, raw := range []string{`{`, `null`} {
		if _, err := images.relaySend(context.Background(), json.RawMessage(raw), nil); err == nil {
			t.Errorf("relaySend accepted %s", raw)
		}
		if _, err := images.relayProbe(context.Background(), json.RawMessage(raw), nil); err == nil {
			t.Errorf("relayProbe accepted %s", raw)
		}
	}
}

// An update with the same set of images, in any order, is not a change and
// leaves the catalog as it was; a different set replaces it.
func TestUpdateCatalogReplacesOnlyADifferentSet(t *testing.T) {
	images, _, _ := testImages(&presenceDriver{})
	if !images.updateCatalog([]string{"a:1", "b:1"}) {
		t.Fatal("the first catalog was not a change")
	}
	if images.updateCatalog([]string{"b:1", "a:1", "a:1"}) {
		t.Fatal("the same images in another order were a change")
	}
	if got := strings.Join(images.catalog(), ","); got != "a:1,b:1" {
		t.Fatalf("catalog after an unchanged update = %s, want a:1,b:1", got)
	}
	if !images.updateCatalog([]string{"a:1"}) {
		t.Fatal("dropping an image was not a change")
	}
	if got := strings.Join(images.catalog(), ","); got != "a:1" {
		t.Fatalf("catalog after dropping b:1 = %s", got)
	}
	if images.updateCatalog([]string{"a:1"}) {
		t.Fatal("an unchanged set was a change")
	}
	if !images.updateCatalog(nil) {
		t.Fatal("emptying the catalog was not a change")
	}
}

// A full sync replaces the catalog as sent, and the caller cannot reach the
// held slice through what catalog returns.
func TestReplaceCatalogKeepsWhatWasSent(t *testing.T) {
	images, _, _ := testImages(&presenceDriver{})
	sent := []string{"b:1", "a:1"}
	images.replaceCatalog(sent)
	sent[0] = "changed"
	got := images.catalog()
	if strings.Join(got, ",") != "b:1,a:1" {
		t.Fatalf("catalog = %v, want what was sent", got)
	}
	got[0] = "changed"
	if images.catalog()[0] != "b:1" {
		t.Fatal("catalog handed out its own slice")
	}
}

// The inventory asks about the catalog when no image is named, says an image
// is present once, and says it again for an image that left the catalog and
// came back.
func TestPresenceLogsAnImageOnceUntilItLeavesTheCatalog(t *testing.T) {
	images, _, logged := testImages(&presenceDriver{exists: true})
	images.replaceCatalog([]string{"a:1", "b:1"})
	present := func() int { return strings.Count(logged(), "image present") }

	if got := images.presence(context.Background(), nil); len(got) != 2 || !got[0].Present {
		t.Fatalf("presence of the catalog = %+v, want both images present", got)
	}
	if got := images.presence(context.Background(), []string{"a:1"}); len(got) != 1 || got[0].ImageRef != "a:1" {
		t.Fatalf("presence of a named image = %+v", got)
	}
	if present() != 2 {
		t.Fatalf("image present logged %d times after two rounds, want once per image", present())
	}
	images.replaceCatalog([]string{"a:1"})
	images.replaceCatalog([]string{"a:1", "b:1"})
	images.presence(context.Background(), nil)
	if present() != 3 {
		t.Fatalf("image present logged %d times, want b:1 said again after it left and returned", present())
	}
}

func TestPresenceReportsNothingWithoutARuntime(t *testing.T) {
	images, _, _ := testImages(nil)
	images.replaceCatalog([]string{"a:1"})
	if got := images.presence(context.Background(), nil); got != nil {
		t.Fatalf("a node with no runtime reported %+v", got)
	}
}
