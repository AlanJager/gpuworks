package app

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestNodeLogUnitsIncludeVendorDrivers(t *testing.T) {
	base := []string{"docker", "containerd"}
	if got := nodeLogUnits(""); len(got) != len(base) {
		t.Fatalf("units for unknown vendor = %v", got)
	}
	nv := nodeLogUnits(accelerator.VendorNVIDIA)
	if !contains(nv, "nvidia-persistenced") {
		t.Fatalf("nvidia units = %v, want the driver unit", nv)
	}
	asc := nodeLogUnits(accelerator.VendorAscend)
	if !contains(asc, "ascend-dmi-service") {
		t.Fatalf("ascend units = %v, want the driver unit", asc)
	}
	if contains(nodeLogUnits(accelerator.VendorAscend), "nvidia-persistenced") {
		t.Fatal("vendor units must not cross over")
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TestFollowNodeLogsStampsSourceAndCapsRate pins the node-log contract: every
// shipped line carries source=node with a parsed level, and a journald storm
// is capped to the per-minute rate instead of flooding the server.
func TestFollowNodeLogsStampsSourceAndCapsRate(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, t.TempDir(), "none", metricsOnlyProber{}, nil)

	var reader io.ReadCloser = io.NopCloser(strings.NewReader(strings.Repeat("docker daemon warning: disk pressure\n", nodeLogRatePerMin*2)))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// The scanner drains 240 lines; the rate cap admits the first 120 and
	// drops the rest. Bind before starting so no batch is sent while offline.
	batches := make(chan proto.LogBatch, 512)
	n.Bind(func(kind string, body any) error {
		if kind == proto.TLogs {
			batches <- body.(proto.LogBatch)
		}
		return nil
	})
	go func() {
		defer close(batches)
		n.followNodeLogs(ctx, reader)
	}()

	// EOF flushes the final batch before the producer closes the channel.
	// Drain that channel completely: a separate done signal can win select
	// while buffered batches still remain, producing a false undercount.
	shipped := 0
	count := func(b proto.LogBatch) {
		for _, l := range b.Lines {
			shipped++
			if l.Source != "node" {
				t.Fatalf("shipped line source = %q, want node", l.Source)
			}
			if l.Level == "" || l.Instance != "" {
				t.Fatalf("node line missing level or carrying an instance: %+v", l)
			}
		}
	}
	deadline := time.After(10 * time.Second)
	drained := false
	for !drained {
		select {
		case b, ok := <-batches:
			if !ok {
				drained = true
				continue
			}
			count(b)
		case <-deadline:
			t.Fatalf("timeout: shipped=%d dropped=%d", shipped, n.logNodeDropped.Load())
		}
	}
	dropped := int(n.logNodeDropped.Load())
	if shipped != nodeLogRatePerMin {
		t.Fatalf("shipped = %d, want the %d rate cap", shipped, nodeLogRatePerMin)
	}
	if dropped < nodeLogRatePerMin {
		t.Fatalf("dropped = %d, want the storm's remainder", dropped)
	}
}
