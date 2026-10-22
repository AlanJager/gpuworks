package conn

import (
	"context"
	"log/slog"
	"net"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestStatusReportsHostListenersInsideTheEnginePortRange(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	ctrl := &nopRWC{}
	c := New(Credentials{NodeUUID: "node-1"}, nil, &inboundHandlers{}, &gpu.FixedProber{VendorName: "test"}, slog.Default())
	c.ctrl = ctrl
	c.enginePorts.Store(&proto.PortRange{Start: port, End: port})
	c.reportStatus(context.Background())
	statuses := sentStatuses(t, ctrl)
	if len(statuses) != 1 || statuses[0].Network == nil {
		t.Fatalf("statuses = %+v, want one with a network inventory", statuses)
	}
	if got := statuses[0].Network.EnginePortListeners; !slices.Equal(got, []int{port}) {
		t.Fatalf("engine port listeners = %v, want [%d]", got, port)
	}
}
