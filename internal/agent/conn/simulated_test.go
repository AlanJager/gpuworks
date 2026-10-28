package conn

import (
	"context"
	"log/slog"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// simulatedProber stands for another machine whose only dialable local
// port is its engine's.
type simulatedProber struct {
	capabilityProber
	engine string
}

func (simulatedProber) SimulatesHost() bool        { return true }
func (p simulatedProber) MayDial(addr string) bool { return addr == p.engine }

func TestSimulatedNodeServesOnlyItsEnginesLogsAndFiles(t *testing.T) {
	p := simulatedProber{capabilityProber: capabilityProber{vendor: "nvidia"}, engine: "127.0.0.1:30001"}
	for _, tc := range []struct {
		hdr  proto.StreamHeader
		want bool
	}{
		{proto.StreamHeader{Kind: proto.StreamTunnel, Dst: "127.0.0.1:30001"}, true},
		{proto.StreamHeader{Kind: proto.StreamTunnel, Dst: "127.0.0.1:8800"}, false},
		{proto.StreamHeader{Kind: proto.StreamTunnel, Dst: "10.0.0.1:22"}, false},
		{proto.StreamHeader{Kind: proto.StreamLogtail}, true},
		{proto.StreamHeader{Kind: proto.StreamLargeFileRead, Dst: "/etc/passwd"}, true}, // confined where it is read (app.StreamLargeFile)
		{proto.StreamHeader{Kind: proto.StreamTerminal}, false},
		{proto.StreamHeader{Kind: proto.StreamModelRead}, false},
		{proto.StreamHeader{Kind: proto.StreamModelWrite}, false},
		{proto.StreamHeader{Kind: proto.StreamImageRead}, false},
		{proto.StreamHeader{Kind: proto.StreamImageWrite}, false},
		{proto.StreamHeader{Kind: proto.StreamAgentUpgrade}, false},
		{proto.StreamHeader{Kind: proto.StreamStorageUpload}, false},
		{proto.StreamHeader{Kind: proto.StreamStorageDownload}, false},
		{proto.StreamHeader{Kind: "future"}, false},
	} {
		if got := simulatedStream(p, tc.hdr); got != tc.want {
			t.Errorf("%s %q: served = %v, want %v", tc.hdr.Kind, tc.hdr.Dst, got, tc.want)
		}
	}
}

func TestSimulatedNodeOffersNoTerminalOrUpgrade(t *testing.T) {
	for _, c := range helloCapabilities(simulatedProber{capabilityProber: capabilityProber{vendor: "nvidia"}}) {
		if c == proto.CapWebterm || c == proto.CapUpgradePush {
			t.Fatalf("simulated node offers %s", c)
		}
	}
}

// slicedProber reports every engine port outside its own slice as taken
// (gpu.ListenerReporter).
type slicedProber struct{ simulatedProber }

func (slicedProber) EnginePortListeners(r proto.PortRange) []int {
	var out []int
	for p := r.Start; p <= r.End; p++ {
		if p%2 == 1 {
			out = append(out, p)
		}
	}
	return out
}

func TestSimulatedInventoryCarriesTheOtherNodesEnginePorts(t *testing.T) {
	ctrl := &nopRWC{}
	c := New(Credentials{NodeUUID: "node-1"}, nil, &inventoryHandlers{}, slicedProber{}, slog.Default())
	c.ctrl = ctrl
	c.enginePorts.Store(&proto.PortRange{Start: 30000, End: 30005})
	c.reportStatus(context.Background())
	statuses := sentStatuses(t, ctrl)
	if len(statuses) != 1 || statuses[0].Network == nil {
		t.Fatalf("statuses = %+v", statuses)
	}
	if got := statuses[0].Network.EnginePortListeners; len(got) != 3 || got[0] != 30001 || got[2] != 30005 {
		t.Fatalf("listeners = %v, want the prober's", got)
	}
	if statuses[0].Network.Interfaces[0].Name != "sim0" {
		t.Fatalf("simulated inventory replaced: %+v", statuses[0].Network)
	}

	// A hermetic agent that is not a simulated node keeps reporting none.
	ctrl = &nopRWC{}
	c = New(Credentials{NodeUUID: "node-1"}, nil, &inventoryHandlers{}, &gpu.FixedProber{VendorName: "test"}, slog.Default())
	c.ctrl = ctrl
	c.enginePorts.Store(&proto.PortRange{Start: 30000, End: 30005})
	c.reportStatus(context.Background())
	if got := sentStatuses(t, ctrl)[0].Network.EnginePortListeners; got != nil {
		t.Fatalf("hermetic listeners = %v, want none", got)
	}
}
