package conn

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

type heldImageSnapshot struct {
	*inboundHandlers
	present bool
	started chan struct{}
	release chan struct{}
}

func (h *heldImageSnapshot) StatusSnapshot(ctx context.Context, _ []string) ([]proto.ImagePresence, []proto.PathUsage) {
	snapshot := []proto.ImagePresence{{ImageRef: "owned:test", Present: h.present}}
	if h.started != nil {
		close(h.started)
		select {
		case <-h.release:
		case <-ctx.Done():
		}
	}
	return snapshot, []proto.PathUsage{{Path: "/owned"}}
}

func TestImageSnapshotCannotOverwriteLaterOperation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		oldPresent bool
		state      string
	}{
		{"completed load", false, "present"},
		{"completed remove", true, "absent"},
		{"failed transfer", true, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			h := &heldImageSnapshot{inboundHandlers: &inboundHandlers{}, present: tc.oldPresent, started: make(chan struct{}), release: make(chan struct{})}
			defer close(h.release)
			ctrl := &nopRWC{}
			c := &Conn{handlers: h, ctrl: ctrl, log: slog.Default(), prober: &gpu.FixedProber{VendorName: "test"}}
			done := make(chan struct{})
			go func() { c.reportStatus(ctx); close(done) }()
			select {
			case <-h.started:
			case <-ctx.Done():
				t.Fatal("snapshot did not start")
			}
			// The operation must be publishable while inventory collection is held.
			sent := make(chan error, 1)
			go func() {
				sent <- c.send(proto.TStatusImageNode, 0, proto.ImageNodeStatus{ImageRef: "owned:test", State: tc.state})
			}()
			select {
			case err := <-sent:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("image operation blocked behind inventory")
			}
			h.release <- struct{}{}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("snapshot did not finish")
			}
			decoder := json.NewDecoder(bytes.NewReader(ctrl.BytesCopy()))
			var event, snapshot proto.Envelope
			if err := decoder.Decode(&event); err != nil {
				t.Fatal(err)
			}
			if err := decoder.Decode(&snapshot); err != nil {
				t.Fatal(err)
			}
			if event.T != proto.TStatusImageNode || snapshot.T != proto.TStatusNode {
				t.Fatalf("wire order: %s, %s", event.T, snapshot.T)
			}
			var status proto.NodeStatus
			if err := json.Unmarshal(snapshot.D, &status); err != nil {
				t.Fatal(err)
			}
			if len(status.Images) != 0 {
				t.Fatalf("older snapshot would overwrite %s: %+v", tc.state, status.Images)
			}
			if len(status.PathUsage) != 1 || status.PathUsage[0].Path != "/owned" || status.Network == nil || status.GPUProbeOK == nil {
				t.Fatalf("other node facts lost: %+v", status)
			}
			if err := decoder.Decode(&event); err != io.EOF {
				t.Fatalf("unexpected trailing message: %v", err)
			}
			// Suppression is limited to the in-flight snapshot. A fresh absence is
			// still authoritative, including after a previous successful load.
			h.started = nil
			h.present = false
			c.reportStatus(ctx)
			decoder = json.NewDecoder(bytes.NewReader(ctrl.BytesCopy()))
			for i := 0; i < 3; i++ {
				if err := decoder.Decode(&snapshot); err != nil {
					t.Fatal(err)
				}
			}
			if err := json.Unmarshal(snapshot.D, &status); err != nil {
				t.Fatal(err)
			}
			if len(status.Images) != 1 || status.Images[0].Present {
				t.Fatalf("fresh absence omitted: %+v", status.Images)
			}
		})
	}
}

func TestUnrelatedControlMessageDoesNotInvalidateImageSnapshot(t *testing.T) {
	ctrl := &nopRWC{}
	c := &Conn{ctrl: ctrl}
	revision := c.imageRevision()
	status := proto.NodeStatus{Images: []proto.ImagePresence{{ImageRef: "owned:test", Present: true}}}
	if err := c.send(proto.TRPCResp, 7, map[string]string{"result": "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := c.sendNodeStatus(status, revision); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(ctrl.BytesCopy()))
	var envelope proto.Envelope
	for i := 0; i < 2; i++ {
		if err := decoder.Decode(&envelope); err != nil {
			t.Fatal(err)
		}
	}
	status = proto.NodeStatus{}
	if err := json.Unmarshal(envelope.D, &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Images) != 1 || !status.Images[0].Present {
		t.Fatalf("fresh presence omitted: %+v", status.Images)
	}
}
