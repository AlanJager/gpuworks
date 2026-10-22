package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
)

const streamLoadCanonical = "registry.example/team/runner:v1"

// streamLoadHarness drives StreamImage(load=true) against the recording
// runtime: the path a server-relayed archive takes on the node.
type streamLoadHarness struct {
	node     *NodeRuntime
	driver   *recording.Driver
	statuses []proto.ImageNodeStatus
}

func newStreamLoadHarness(t *testing.T) *streamLoadHarness {
	t.Helper()
	driver := recording.New(t.TempDir() + "/spec.json")
	driver.InjectInspection(runtime.ImageInspection{ID: "sha256:relayed", Entrypoint: []string{"/entrypoint.sh"}})
	node := NewNodeRuntime(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), driver, t.TempDir()+"/models")
	h := &streamLoadHarness{node: node, driver: driver}
	node.Bind(func(kind string, body any) error {
		if kind == proto.TStatusImageNode {
			h.statuses = append(h.statuses, body.(proto.ImageNodeStatus))
		}
		return nil
	})
	return h
}

func (h *streamLoadHarness) load(t *testing.T) error {
	t.Helper()
	return h.node.StreamImage(context.Background(), streamLoadCanonical, "relay-1", bytes.NewBufferString("relayed-archive"), true)
}

func (h *streamLoadHarness) terminal(t *testing.T) proto.ImageNodeStatus {
	t.Helper()
	if len(h.statuses) == 0 {
		t.Fatal("no image-node status was reported")
	}
	return h.statuses[len(h.statuses)-1]
}

func TestStreamedArchiveThatAlreadyCarriesTheCatalogReferenceIsNotTagged(t *testing.T) {
	h := newStreamLoadHarness(t)

	if err := h.load(t); err != nil {
		t.Fatalf("StreamImage: %v", err)
	}
	status := h.terminal(t)
	if status.State != "present" || status.TransferID != "relay-1" {
		t.Fatalf("status = %+v", status)
	}
	if status.ImageID != "sha256:relayed" {
		t.Fatalf("status = %+v, want the inspected image id", status)
	}
	for _, call := range h.driver.Calls() {
		if strings.HasPrefix(call, "TagImage(") {
			t.Fatalf("an archive that already carried %s was tagged: %v", streamLoadCanonical, h.driver.Calls())
		}
	}
}

func TestStreamedArchiveCarryingAnotherReferenceIsTaggedToTheCatalogOne(t *testing.T) {
	h := newStreamLoadHarness(t)
	const published = "gpustack/runner:cuda13.0-vllm0.25.1"
	h.driver.InjectMissingImage(streamLoadCanonical)
	h.driver.InjectLoadedRef(published)

	if err := h.load(t); err != nil {
		t.Fatalf("StreamImage: %v", err)
	}
	status := h.terminal(t)
	if status.State != "present" {
		t.Fatalf("status = %+v", status)
	}
	if !strings.Contains(status.Message, published) || !strings.Contains(status.Message, streamLoadCanonical) {
		t.Fatalf("message = %q, want both references", status.Message)
	}
	want := []string{
		"LoadImage(" + streamLoadCanonical + ")",
		"ImageExists(" + streamLoadCanonical + ")",
		"TagImage(" + published + "->" + streamLoadCanonical + ")",
		"ImageExists(" + streamLoadCanonical + ")",
		"InspectImage(" + streamLoadCanonical + ")",
	}
	if got := h.driver.Calls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestStreamedArchiveReportsAFailedTag(t *testing.T) {
	h := newStreamLoadHarness(t)
	const published = "gpustack/runner:cuda13.0-vllm0.25.1"
	h.driver.InjectMissingImage(streamLoadCanonical)
	h.driver.InjectLoadedRef(published)
	h.driver.InjectTagFailure(errors.New("no such image"))

	err := h.load(t)
	if err == nil || !strings.Contains(err.Error(), "tag "+published+" as "+streamLoadCanonical) {
		t.Fatalf("error = %v", err)
	}
	status := h.terminal(t)
	if status.State != "failed" || !strings.Contains(status.Message, "no such image") {
		t.Fatalf("status = %+v", status)
	}
}

func TestStreamedArchiveThatNamesNoReferenceFailsWithTheReasonSaidOutLoud(t *testing.T) {
	h := newStreamLoadHarness(t)
	h.driver.InjectMissingImage(streamLoadCanonical)
	h.driver.InjectNoLoadedRef()

	err := h.load(t)
	if err == nil || !strings.Contains(err.Error(), "did not report the reference the archive carried") {
		t.Fatalf("error = %v", err)
	}
	status := h.terminal(t)
	if status.State != "failed" || status.TransferID != "relay-1" {
		t.Fatalf("status = %+v", status)
	}
	for _, call := range h.driver.Calls() {
		if strings.HasPrefix(call, "TagImage(") {
			t.Fatalf("a load that named no reference still tried to tag: %v", h.driver.Calls())
		}
	}
}
