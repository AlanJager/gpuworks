package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/nettest"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// networkTestHub answers net.bench.* and net.probe like the agents do, and
// records which source IP each probe was bound to.
type networkTestHub struct {
	*distributedTestHub
	mu        sync.Mutex
	ipOwner   map[string]string
	sourceIPs map[string]string
}

func (h *networkTestHub) CallNode(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
	raw, _ := json.Marshal(params)
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	switch method {
	case proto.RPCNetBenchStart:
		return json.RawMessage(`{"port":41000}`), nil
	case proto.RPCNetBenchStop:
		return json.RawMessage(`{"stopped":true}`), nil
	case proto.RPCNetProbe:
		host, _, _ := net.SplitHostPort(p["target"].(string))
		h.mu.Lock()
		h.sourceIPs[uuid+"->"+h.ipOwner[host]] = fmt.Sprint(p["source_ip"])
		h.mu.Unlock()
		return json.RawMessage(`{"reachable":true,"rtt_ms_p50":0.1,"rtt_ms_p99":0.3,"gbps":9.5,"bandwidth_tested":true}`), nil
	}
	return nil, fmt.Errorf("unexpected %s", method)
}

func networkTestStore(t *testing.T, name string) (*store.Store, *store.Network) {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for i, uuid := range []string{"n1", "n2", "n3"} {
		if _, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
		inv := proto.NetInventory{Interfaces: []proto.NetInterface{{Name: "ens3f0", Addrs: []string{fmt.Sprintf("10.30.1.%d/16", i+1)}}}}
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{Network: &inv}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetNodeLabels(uuid, map[string]string{"rack": "r1"}); err != nil {
			t.Fatal(err)
		}
	}
	lan, err := st.CreateNetwork(&store.Network{Name: "lan", Type: netplan.TypeHostEthernet, CIDR: "10.30.0.0/16", Roles: store.EncodeStringList([]string{netplan.RoleStorageSync})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateNetworkAttachment(&store.NetworkAttachment{NetworkID: lan.ID, LabelKey: "rack", LabelValue: "r1"}); err != nil {
		t.Fatal(err)
	}
	return st, lan
}

func waitNetworkTest(t *testing.T, st *store.Store, networkID uint) *store.NetworkTestRun {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if run, err := st.LatestNetworkTestRun(networkID); err == nil && run.Status != store.NetworkTestRunning {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("network test did not finish")
	return nil
}

func TestControllerRunsRequestedNetworkTests(t *testing.T) {
	st, lan := networkTestStore(t, "controller_network_test_run")
	hub := &networkTestHub{
		distributedTestHub: &distributedTestHub{online: map[string]bool{"n1": true, "n2": true}, updates: map[string][]proto.StateUpdate{}},
		ipOwner:            map[string]string{"10.30.1.1": "n1", "10.30.1.2": "n2", "10.30.1.3": "n3"},
		sourceIPs:          map[string]string{},
	}
	c := New(st, hub, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	time.Sleep(50 * time.Millisecond) // let Run subscribe before the request is published

	if _, err := st.StartNetworkTestRun(&store.NetworkTestRun{NetworkID: lan.ID, Scope: "all", Bandwidth: true, Seconds: 1, Nodes: `["n1","n2","n3"]`, NodeCount: 3}); err != nil {
		t.Fatal(err)
	}
	run := waitNetworkTest(t, st, lan.ID)
	var pairs []struct {
		Source string  `json:"source_uuid"`
		Target string  `json:"target_uuid"`
		Gbps   float64 `json:"gbps"`
		Error  string  `json:"error"`
	}
	if err := json.Unmarshal([]byte(run.Pairs), &pairs); err != nil {
		t.Fatal(err)
	}
	if run.Status != store.NetworkTestDone || run.PairCount != 6 || run.PassedCount != 2 || len(pairs) != 6 {
		t.Fatalf("run = %+v", run)
	}
	for _, p := range pairs {
		healthy := p.Source != "n3" && p.Target != "n3"
		if healthy && (p.Error != "" || p.Gbps != 9.5) {
			t.Fatalf("pair = %+v", p)
		}
		if !healthy && p.Error != "target is offline" && p.Error != "source is offline" {
			t.Fatalf("offline pair = %+v", p)
		}
	}
	if got := hub.sourceIPs["n1->n2"]; got != "10.30.1.1" {
		t.Fatalf("probe bound to %q, want the node's address on the network", got)
	}
}

func TestControllerAbandonsNetworkTestsWithoutNodeRPC(t *testing.T) {
	st, lan := networkTestStore(t, "controller_network_test_no_rpc")
	// A run left over from a previous process is closed when Run starts.
	stale, err := st.StartNetworkTestRun(&store.NetworkTestRun{NetworkID: lan.ID, Nodes: `["n1","n2"]`})
	if err != nil {
		t.Fatal(err)
	}
	c := New(st, &distributedTestHub{online: map[string]bool{}, updates: map[string][]proto.StateUpdate{}}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		run, _ := st.NetworkTestRunByID(stale.ID)
		if run.Status == store.NetworkTestInterrupted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stale run status = %s", run.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := st.StartNetworkTestRun(&store.NetworkTestRun{NetworkID: lan.ID, Nodes: `["n1","n2"]`}); err != nil {
		t.Fatal(err)
	}
	if run := waitNetworkTest(t, st, lan.ID); run.Status != store.NetworkTestInterrupted || run.FinishedAt == nil {
		t.Fatalf("run without node RPC = %+v", run)
	}
}

func TestNetworkTestEndpointsReportMissingNodesAndAddresses(t *testing.T) {
	st, lan := networkTestStore(t, "controller_network_test_endpoints")
	c := New(st, &distributedTestHub{online: map[string]bool{"n1": true, "n2": true, "n3": true}, updates: map[string][]proto.StateUpdate{}}, nil)
	// n2 leaves the network, a deleted node stays in the run's list.
	if _, err := st.SetNodeLabels("n2", map[string]string{"rack": "other"}); err != nil {
		t.Fatal(err)
	}
	eps, err := c.networkTestEndpoints(&store.NetworkTestRun{NetworkID: lan.ID, Nodes: `["n1","n2","gone"]`})
	if err != nil {
		t.Fatal(err)
	}
	if eps[0].IP != "10.30.1.1" || eps[1].IP != "" || eps[1].Unavailable != "" || eps[2].Unavailable != nettest.CodeDeleted {
		t.Fatalf("endpoints = %+v", eps)
	}
	if _, err := c.networkTestEndpoints(&store.NetworkTestRun{NetworkID: lan.ID, Nodes: `{`}); err == nil {
		t.Fatal("corrupt node list accepted")
	}
}

func TestRDMAToolEndpointsCarryRailsAndGaps(t *testing.T) {
	st, err := store.Open("file:controller_rdma_tool_endpoints?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for i, uuid := range []string{"with-tool", "no-tool", "no-ports"} {
		if _, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
		inv := proto.NetInventory{Interfaces: []proto.NetInterface{{Name: "eth0", Addrs: []string{fmt.Sprintf("10.96.0.%d/32", i+1)}}}}
		if uuid != "no-ports" {
			inv.RDMAPorts = []proto.RDMAPort{{Device: "mlx5_0", Port: 1, State: "ACTIVE", LinkLayer: "InfiniBand"}}
		}
		if uuid == "with-tool" {
			inv.Tools = []string{"ib_write_bw"}
		}
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{Network: &inv}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetNodeLabels(uuid, map[string]string{"pool": "ib"}); err != nil {
			t.Fatal(err)
		}
	}
	ib, _ := st.CreateNetwork(&store.Network{Name: "ib", Type: netplan.TypeHostRDMA, CIDR: "10.96.0.0/13", Roles: store.EncodeStringList([]string{netplan.RoleCompute})})
	if _, err := st.CreateNetworkAttachment(&store.NetworkAttachment{NetworkID: ib.ID, LabelKey: "pool", LabelValue: "ib"}); err != nil {
		t.Fatal(err)
	}
	c := New(st, &distributedTestHub{online: map[string]bool{"with-tool": true, "no-tool": true, "no-ports": true}, updates: map[string][]proto.StateUpdate{}}, nil)
	eps, err := c.networkTestEndpoints(&store.NetworkTestRun{NetworkID: ib.ID, Kind: nettest.ModeRDMATool, Nodes: `["with-tool","no-tool","no-ports"]`})
	if err != nil {
		t.Fatal(err)
	}
	if eps[0].RDMAUnavailable != "" || !reflect.DeepEqual(eps[0].Rails, []nettest.Rail{{Name: "mlx5_0:1"}}) || eps[0].IP != "10.96.0.1" {
		t.Fatalf("with tool = %+v", eps[0])
	}
	if eps[1].RDMAUnavailable != nettest.CodeToolMissing {
		t.Fatalf("no tool = %+v", eps[1])
	}
	if eps[2].RDMAUnavailable != nettest.CodeNoHCA {
		t.Fatalf("no ports = %+v", eps[2])
	}
	// A TCP run does not care about tools.
	eps, _ = c.networkTestEndpoints(&store.NetworkTestRun{NetworkID: ib.ID, Nodes: `["no-tool"]`})
	if eps[0].RDMAUnavailable != "" {
		t.Fatalf("tcp endpoint = %+v", eps[0])
	}
}

// The management network has no row: its members are the nodes that reported
// a management address, tested at that address even where dedicated networks
// carry every role.
func TestManagementNetworkTestEndpoints(t *testing.T) {
	st, err := store.Open("file:controller_management_test_endpoints?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, uuid := range []string{"m1", "m2", "old"} {
		if _, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
	}
	for uuid, ip := range map[string]string{"m1": "172.25.0.1", "m2": "172.25.0.2"} {
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{Network: &proto.NetInventory{
			Interfaces: []proto.NetInterface{{Name: "eth0", Addrs: []string{ip + "/24"}}},
			Management: &proto.ManagementAddress{Interface: "eth0", IP: ip},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	// A dedicated network takes every role from m1's management network.
	all, _ := st.CreateNetwork(&store.Network{Name: "all", Type: netplan.TypeHostEthernet, CIDR: "172.25.0.0/24", Roles: store.EncodeStringList(netplan.Roles)})
	m1, _ := st.NodeByUUID("m1")
	if _, err := st.CreateNetworkAttachment(&store.NetworkAttachment{NetworkID: all.ID, NodeID: m1.ID}); err != nil {
		t.Fatal(err)
	}
	c := New(st, &distributedTestHub{online: map[string]bool{"m1": true, "m2": true, "old": true}, updates: map[string][]proto.StateUpdate{}}, nil)
	eps, err := c.networkTestEndpoints(&store.NetworkTestRun{NetworkID: 0, Nodes: `["m1","m2","old"]`})
	if err != nil {
		t.Fatal(err)
	}
	if eps[0].IP != "172.25.0.1" || eps[1].IP != "172.25.0.2" || eps[2].IP != "" {
		t.Fatalf("tcp endpoints = %+v", eps)
	}
}

// A ring runs in node-name order whatever order the run stored its nodes in,
// and RoCE rails carry their interface addresses for pairing by subnet.
func TestRingEndpointsRunInNameOrder(t *testing.T) {
	st, err := store.Open("file:controller_ring_endpoints?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for i, name := range []string{"node-c", "node-a", "node-b"} {
		uuid := fmt.Sprintf("u%d", i)
		if _, err := st.RegisterNode(uuid, store.HashToken(uuid), name); err != nil {
			t.Fatal(err)
		}
		inv := proto.NetInventory{
			Interfaces: []proto.NetInterface{{Name: "enp61s0f0", SpeedMbps: 25000, Addrs: []string{fmt.Sprintf("100.64.29.%d/24", i+1)}}},
			RDMAPorts:  []proto.RDMAPort{{Device: "rocep61s0f0", Port: 1, State: "ACTIVE", LinkLayer: "Ethernet", NetDev: "enp61s0f0"}},
			Tools:      []string{"ib_write_bw"},
		}
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{Network: &inv}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetNodeLabels(uuid, map[string]string{"pool": "roce"}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.RenameNode(uuid, name); err != nil {
			t.Fatal(err)
		}
	}
	roce, _ := st.CreateNetwork(&store.Network{Name: "roce", Type: netplan.TypeHostRDMA, Roles: store.EncodeStringList([]string{netplan.RoleCompute})})
	if _, err := st.CreateNetworkAttachment(&store.NetworkAttachment{NetworkID: roce.ID, LabelKey: "pool", LabelValue: "roce"}); err != nil {
		t.Fatal(err)
	}
	c := New(st, &distributedTestHub{online: map[string]bool{"u0": true, "u1": true, "u2": true}, updates: map[string][]proto.StateUpdate{}}, nil)
	eps, err := c.networkTestEndpoints(&store.NetworkTestRun{NetworkID: roce.ID, Kind: nettest.ModeRDMATool, Pattern: nettest.PatternRing, Nodes: `["u0","u1","u2"]`})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, ep := range eps {
		order = append(order, ep.NodeUUID)
	}
	if !reflect.DeepEqual(order, []string{"u1", "u2", "u0"}) {
		t.Fatalf("ring order = %v", order)
	}
	if want := []nettest.Rail{{Name: "rocep61s0f0:1", Addr: "100.64.29.2/24"}}; !reflect.DeepEqual(eps[0].Rails, want) || eps[0].IP != "100.64.29.2" || eps[0].RDMAUnavailable != "" {
		t.Fatalf("endpoint = %+v", eps[0])
	}
}

// cardTestHub answers accelerator.ping for two hosts cabled card to card:
// a card reaches only the card of the same name on the other host.
type cardTestHub struct {
	*distributedTestHub
	mu    sync.Mutex
	owner map[string]string // card address -> card name
	calls map[string]int    // node -> accelerator.ping calls
}

func (h *cardTestHub) CallNode(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
	if method != proto.RPCAcceleratorPing {
		return nil, fmt.Errorf("unexpected %s", method)
	}
	raw, _ := json.Marshal(params)
	var p proto.AcceleratorPingParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.calls[uuid]++
	h.mu.Unlock()
	out := make([]proto.AcceleratorPingResult, len(p.Probes))
	for i, probe := range p.Probes {
		out[i] = proto.AcceleratorPingResult{Card: probe.Card, Target: probe.Target}
		if h.owner[probe.Target] == probe.Card {
			out[i].Reachable, out[i].RTTMS = true, 0.3
		} else {
			out[i].Code, out[i].Error = proto.AcceleratorPingUnreachable, "no answer within the time limit"
		}
	}
	return json.Marshal(out)
}

func TestControllerRunsACardMatrix(t *testing.T) {
	st, err := store.Open("file:controller_card_matrix?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	network, err := st.CreateNetwork(&store.Network{Name: "npu", Type: netplan.TypeAcceleratorRDMA, Roles: store.EncodeStringList([]string{netplan.RoleCompute})})
	if err != nil {
		t.Fatal(err)
	}
	hub := &cardTestHub{
		distributedTestHub: &distributedTestHub{online: map[string]bool{"h31": true, "h32": true}, updates: map[string][]proto.StateUpdate{}},
		owner:              map[string]string{}, calls: map[string]int{},
	}
	for i, uuid := range []string{"h31", "h32"} {
		node, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		inv := proto.NetInventory{
			Interfaces: []proto.NetInterface{{Name: "br0", Addrs: []string{fmt.Sprintf("198.51.100.%d/16", 31+i)}}},
			Management: &proto.ManagementAddress{Interface: "br0", IP: fmt.Sprintf("198.51.100.%d", 31+i)},
		}
		for _, card := range []int{3, 4} {
			ip := fmt.Sprintf("203.0.113.%d", 10*i+card)
			inv.AcceleratorPorts = append(inv.AcceleratorPorts, proto.AcceleratorPort{Name: fmt.Sprintf("npu%d", card), Index: card, IP: ip, Netmask: "255.255.0.0", State: "UP", SpeedGbps: 200})
			hub.owner[ip] = fmt.Sprintf("npu%d", card)
		}
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{Network: &inv}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateNetworkAttachment(&store.NetworkAttachment{NetworkID: network.ID, NodeID: node.ID}); err != nil {
			t.Fatal(err)
		}
	}
	c := New(st, hub, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	time.Sleep(50 * time.Millisecond) // let Run subscribe before the request is published

	if _, err := st.StartNetworkTestRun(&store.NetworkTestRun{NetworkID: network.ID, Kind: nettest.ModeCardMatrix, Pattern: nettest.PatternMesh, Scope: "all", Nodes: `["h31","h32"]`, NodeCount: 2}); err != nil {
		t.Fatal(err)
	}
	run := waitNetworkTest(t, st, network.ID)
	var pairs []nettest.Pair
	if err := json.Unmarshal([]byte(run.Pairs), &pairs); err != nil {
		t.Fatal(err)
	}
	// One node pair whose same-named cards reach each other and no others:
	// the diagonal, a usable pattern, so the pair counts as passed.
	if run.Status != store.NetworkTestDone || run.PairCount != 1 || run.PassedCount != 1 || len(pairs) != 1 || nettest.CardShape(pairs[0]) != nettest.ShapeDiagonal {
		t.Fatalf("run = %+v", run)
	}
	var got []string
	for _, r := range pairs[0].Rails {
		got = append(got, fmt.Sprintf("%s>%s:%v", r.Rail, r.TargetRail, r.OK()))
	}
	if want := []string{"npu3>npu3:true", "npu3>npu4:false", "npu4>npu3:false", "npu4>npu4:true"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("matrix = %v, want %v", got, want)
	}
	if pairs[0].Rails[0].SourceIP != "203.0.113.3" || pairs[0].Rails[0].TargetIP != "203.0.113.13" || pairs[0].Target != "198.51.100.32" {
		t.Fatalf("pair = %+v", pairs[0])
	}
	// Two shifts, each one call on the source node only.
	if !reflect.DeepEqual(hub.calls, map[string]int{"h31": 2}) {
		t.Fatalf("calls = %v", hub.calls)
	}
}
