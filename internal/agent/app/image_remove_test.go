package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
)

func imageRemoveNode(t *testing.T) (*NodeRuntime, *recording.Driver, chan proto.ImageNodeStatus) {
	t.Helper()
	driver := recording.New(t.TempDir() + "/spec.json")
	node := NewNodeRuntime(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), driver, t.TempDir()+"/models")
	statuses := make(chan proto.ImageNodeStatus, 8)
	node.send = func(kind string, data any) error {
		if kind == proto.TStatusImageNode {
			statuses <- data.(proto.ImageNodeStatus)
		}
		return nil
	}
	return node, driver, statuses
}

// Deleting an image is the operator's way to give a node's disk back, so the
// node must ask its runtime and tell the server the row is empty again.
func TestRPCImageRemoveDeletesTheImageAndReportsAbsent(t *testing.T) {
	node, driver, statuses := imageRemoveNode(t)
	const ref = "gpustack/runner:cann8.5-910b-vllm0.18.0"
	raw, err := node.rpcImageRemove(context.Background(), json.RawMessage(`{"image_ref":"`+ref+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	result, ok := raw.(proto.ImageRemoveResult)
	if !ok || result.State != "absent" || result.ImageRef != ref {
		t.Fatalf("result = %#v", raw)
	}
	if calls := driver.Calls(); len(calls) != 1 || calls[0] != "RemoveImage("+ref+")" {
		t.Fatalf("runtime calls = %v", calls)
	}
	select {
	case status := <-statuses:
		if status.State != "absent" || status.ImageRef != ref {
			t.Fatalf("reported status = %+v", status)
		}
	default:
		t.Fatal("the node never told the server the image is gone")
	}
}

// A runtime that cannot delete the image must say so rather than pretend the
// row is empty: the image is still taking up the node's disk.
func TestRPCImageRemoveReportsARuntimeRefusal(t *testing.T) {
	node, driver, statuses := imageRemoveNode(t)
	refusal := errors.New("conflict: unable to delete image (must be forced) - image is being used by running container")
	driver.InjectRemoveFailure(refusal)
	if _, err := node.rpcImageRemove(context.Background(), json.RawMessage(`{"image_ref":"registry.example/team/runner:v1"}`)); !errors.Is(err, refusal) {
		t.Fatalf("err = %v, want the runtime's own refusal", err)
	}
	if len(statuses) != 0 {
		t.Fatalf("a refused deletion must not change the row: %+v", <-statuses)
	}
}

func TestRPCImageRemoveRequiresAReference(t *testing.T) {
	node, _, _ := imageRemoveNode(t)
	if _, err := node.rpcImageRemove(context.Background(), json.RawMessage(`{"image_ref":"  "}`)); err == nil ||
		!strings.Contains(err.Error(), "image_ref") {
		t.Fatalf("err = %v", err)
	}
}
