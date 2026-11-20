package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// peersCapsHub is a distributed test hub whose agents advertise caps.
type peersCapsHub struct {
	*distributedTestHub
	caps map[string][]string
}

func (h *peersCapsHub) NodeCaps(uuid string) []string { return h.caps[uuid] }

// Every rank of a multi-node generation on a RoCE network launches with the
// same peers file - each rank, its address and both of its links with their
// own addresses - and the variable that names it. A rank whose agent does not
// write peers files launches without either, while its peers still list it.
func TestMultiNodeRanksLaunchWithTheGroupsPeersFile(t *testing.T) {
	st, err := store.Open("file:peers_file_generation?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "example/peers", Name: "peers", WeightBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"gb10-1", "gb10-2", "gb10-3"}
	var nodes []*store.Node
	var ids []uint
	for i, uuid := range names {
		node, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		a, b, mgmt := fmt.Sprintf("192.168.177.%d", 51+i), fmt.Sprintf("192.168.178.%d", 51+i), fmt.Sprintf("10.0.0.%d", 51+i)
		inv := proto.NetInventory{
			Interfaces: []proto.NetInterface{{Name: "enp1s0f1np1", Addrs: []string{a + "/24"}}, {Name: "enP2p1s0f1np1", Addrs: []string{b + "/24"}}, {Name: "eno1", Addrs: []string{mgmt + "/24"}}},
			RDMAPorts: []proto.RDMAPort{
				{Device: "rocep1s0f1", Port: 1, State: "ACTIVE", LinkLayer: "Ethernet", NetDev: "enp1s0f1np1", RoCEAddress: &proto.RoCEAddress{NetDev: "enp1s0f1np1", CIDR: a + "/24"}},
				{Device: "roceP2p1s0f1", Port: 1, State: "ACTIVE", LinkLayer: "Ethernet", NetDev: "enP2p1s0f1np1", RoCEAddress: &proto.RoCEAddress{NetDev: "enP2p1s0f1np1", CIDR: b + "/24"}},
			},
			Management: &proto.ManagementAddress{Interface: "eno1", IP: mgmt},
		}
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{Network: &inv}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetNodeLabels(uuid, map[string]string{"pool": "roce"}); err != nil {
			t.Fatal(err)
		}
		if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: uuid + "-gpu", Name: "GB10", Vendor: "nvidia", MemoryTotal: 96 << 30}).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/peers"}); err != nil {
			t.Fatal(err)
		}
		nodes, ids = append(nodes, node), append(ids, node.ID)
	}
	roce, err := st.CreateNetwork(&store.Network{Name: "roce", Type: netplan.TypeHostRDMA, CIDR: "192.168.177.0/24", Roles: store.EncodeStringList([]string{netplan.RoleCompute})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateNetworkAttachment(&store.NetworkAttachment{NetworkID: roce.ID, LabelKey: "pool", LabelValue: "roce"}); err != nil {
		t.Fatal(err)
	}
	nodeIDs, _ := json.Marshal(ids)
	dep, err := createTestDeployment(t, st, &store.Deployment{ModelID: model.ID, NodeID: nodes[0].ID, NodeIDs: string(nodeIDs), ServedName: "peers", Engine: "vllm", State: "active", NNodes: 3, MasterPort: 29500, Placement: `{"tp":3}`})
	if err != nil {
		t.Fatal(err)
	}
	online := map[string]bool{}
	caps := map[string][]string{}
	for _, uuid := range names {
		online[uuid] = true
		caps[uuid] = []string{proto.CapPeersFile}
	}
	caps["gb10-3"] = nil // an agent that predates peers files
	hub := &peersCapsHub{distributedTestHub: &distributedTestHub{online: online, updates: map[string][]proto.StateUpdate{}}, caps: caps}
	c := New(st, hub, nil)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	instances, err := st.InstancesForDeployment(dep.ID)
	if err != nil || len(instances) != 3 {
		t.Fatalf("instances = %+v err=%v", instances, err)
	}
	byNode := map[uint]proto.InstanceDesiredState{}
	for _, inst := range instances {
		desired, err := inst.DesiredState()
		if err != nil {
			t.Fatal(err)
		}
		byNode[inst.NodeID] = desired
	}
	for rank, node := range nodes[:2] {
		desired := byNode[node.ID]
		file := desired.Peers
		if file == nil {
			t.Fatalf("rank %d on %s has no peers file", rank, node.UUID)
		}
		if desired.Env[deploy.PeersFileEnv] != proto.PeersFileMountPath {
			t.Fatalf("rank %d env %s = %q", rank, deploy.PeersFileEnv, desired.Env[deploy.PeersFileEnv])
		}
		if file.Version != proto.PeersFileVersion || file.NodeRank != rank || file.NNodes != 3 || len(file.Ranks) != 3 ||
			file.MasterAddr != "192.168.177.51" || file.MasterPort != 29500 ||
			file.Network == nil || file.Network.Type != netplan.TypeHostRDMA || file.Network.LinkLayer != netplan.LinkLayerRoCE {
			t.Fatalf("rank %d peers file = %+v", rank, file)
		}
		for i, peer := range file.Ranks {
			want := []proto.PeerHCA{
				{Name: "roceP2p1s0f1:1", Device: "roceP2p1s0f1", Port: 1, NetDev: "enP2p1s0f1np1", IP: fmt.Sprintf("192.168.178.%d", 51+i)},
				{Name: "rocep1s0f1:1", Device: "rocep1s0f1", Port: 1, NetDev: "enp1s0f1np1", IP: fmt.Sprintf("192.168.177.%d", 51+i)},
			}
			if peer.NodeRank != i || peer.NodeUUID != names[i] || peer.NodeAddr != fmt.Sprintf("192.168.177.%d", 51+i) ||
				peer.Management == nil || peer.Management.IP != fmt.Sprintf("10.0.0.%d", 51+i) || fmt.Sprint(peer.HCAs) != fmt.Sprint(want) {
				t.Fatalf("rank %d lists peer %d as %+v, want links %+v", rank, i, peer, want)
			}
		}
	}
	old := byNode[nodes[2].ID]
	if old.Peers != nil {
		t.Fatalf("a rank whose agent does not write peers files got one: %+v", old.Peers)
	}
	if _, set := old.Env[deploy.PeersFileEnv]; set {
		t.Fatalf("a rank whose agent does not write peers files names one: %v", old.Env)
	}
}
