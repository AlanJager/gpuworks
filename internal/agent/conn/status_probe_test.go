package conn

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// stalledProber is a prober whose vendor CLI stopped answering.
type stalledProber struct{ gpu.FixedProber }

func (*stalledProber) Probe(context.Context) ([]proto.GPUInfo, error) {
	return nil, fmt.Errorf("nvidia-smi: no answer within 10s: %w", gpu.ErrCLIUnresponsive)
}

type partitionProbeHandlers struct {
	inboundHandlers
	probed [][]proto.GPUInfo
	cached int
}

func (h *partitionProbeHandlers) PartitionProbes(_ context.Context, _ []proto.AssetReport, gpus []proto.GPUInfo) []proto.PartitionProbeResult {
	h.probed = append(h.probed, gpus)
	return []proto.PartitionProbeResult{{Backend: "fresh", Status: proto.PartitionProbePassed}}
}

func (h *partitionProbeHandlers) CachedPartitionProbes() []proto.PartitionProbeResult {
	h.cached++
	return []proto.PartitionProbeResult{{Backend: "kept", Status: proto.PartitionProbePassed}}
}

// While the GPU inventory cannot be read, the node keeps reporting its last
// partition probe results: the missing inventory must not be taken as "no
// GPUs", which would rerun the probes against nothing and wipe them.
func TestReportStatusKeepsPartitionProbesWhileGPUProbeFails(t *testing.T) {
	gpus := []proto.GPUInfo{{Index: 0, Vendor: "nvidia", Name: "A100", Healthy: true}}
	h := &partitionProbeHandlers{}

	ctrl := &nopRWC{}
	c := New(Credentials{NodeUUID: "node-1"}, nil, h, &stalledProber{gpu.FixedProber{VendorName: "nvidia", GPUs: gpus}}, slog.Default())
	c.ctrl = ctrl
	c.reportStatus(context.Background())
	got := string(ctrl.BytesCopy())
	if len(h.probed) != 0 || h.cached != 1 {
		t.Fatalf("stalled probe: PartitionProbes calls=%d cached calls=%d, want 0/1", len(h.probed), h.cached)
	}
	for _, want := range []string{`"gpu_probe_ok":false`, `"backend":"kept"`, "no answer within 10s"} {
		if !strings.Contains(got, want) {
			t.Fatalf("stalled status lacks %s: %s", want, got)
		}
	}

	ctrl = &nopRWC{}
	c = New(Credentials{NodeUUID: "node-1"}, nil, h, &gpu.FixedProber{VendorName: "nvidia", GPUs: gpus}, slog.Default())
	c.ctrl = ctrl
	c.reportStatus(context.Background())
	if len(h.probed) != 1 || len(h.probed[0]) != 1 || h.cached != 1 || !strings.Contains(string(ctrl.BytesCopy()), `"backend":"fresh"`) {
		t.Fatalf("recovered probe: PartitionProbes calls=%v cached calls=%d status=%s", h.probed, h.cached, ctrl.BytesCopy())
	}
}
