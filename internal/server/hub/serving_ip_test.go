package hub

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	agentconn "gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// servingNetworks answers every node with one serving address.
type servingNetworks struct{ ip string }

func (n servingNetworks) StoragePath(uint, uint) (netplan.StoragePath, error) {
	return netplan.StoragePath{}, netplan.ErrNoStoragePath
}
func (n servingNetworks) ServingIP(uint) string { return n.ip }

// stateCapture records the desired state an agent receives.
type stateCapture struct {
	noopHandlers
	states chan proto.DesiredState
}

func (c stateCapture) OnStateSync(_ context.Context, ds proto.DesiredState) { c.states <- ds }

// The store builds the desired state; where the node's serving forwarder
// listens is a network decision the hub asks its network service for, and the
// engine port range is the controller's, read through SetEnginePorts.
func TestInitialStateCarriesTheServingAddressAndEnginePorts(t *testing.T) {
	st, err := store.Open("file:hub_serving_ip_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := New(st, slog.Default())
	h.SetNetworks(servingNetworks{ip: "10.40.0.5"})
	h.SetEnginePorts(func() proto.PortRange { return proto.PortRange{Start: 31000, End: 31005} })
	_, nodeToken, err := st.CreateNodePending("serving")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	capture := stateCapture{states: make(chan proto.DesiredState, 4)}
	done := make(chan error, 1)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		c := agentconn.New(agentconn.Credentials{NodeToken: nodeToken, ConnectMode: "server_dial"},
			func(string, string) error { return nil }, capture, emptyProber{}, slog.Default())
		done <- c.RunAccepted(ctx, raw)
	}()
	if err := h.DialAgentWithSecret(context.Background(), ln.Addr().String(), nodeToken); err != nil {
		t.Fatal(err)
	}
	select {
	case ds := <-capture.states:
		if ds.Network.ServingIP != "10.40.0.5" {
			t.Fatalf("serving IP = %q", ds.Network.ServingIP)
		}
		if got := ds.Network.EnginePorts; got == nil || *got != (proto.PortRange{Start: 31000, End: 31005}) {
			t.Fatalf("engine ports = %+v, want 31000-31005", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no state sync")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("agent session did not exit")
	}
}
