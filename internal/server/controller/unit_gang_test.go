package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A multi-node replica is placed as a whole. While its follower's node has no
// usable card, nothing is written: the leader holds no card, no attempt is
// spent, and the refusal names the rank and node that held the replica back.
// Once the card is back, both ranks are written in one pass.
func TestAMultiNodeReplicaIsWrittenWholeOrNotAtAll(t *testing.T) {
	st, err := store.Open("file:unit_gang_whole?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "example/gang", Name: "gang", WeightBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	var nodes []*store.Node
	for rank, uuid := range []string{"gang-rank-0", "gang-rank-1"} {
		node, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		node.NetworkInventory = managementInventoryJSON(t, fmt.Sprintf("10.3.0.%d", rank+1))
		if err := st.DB().Save(node).Error; err != nil {
			t.Fatal(err)
		}
		// The follower's only card is unhealthy at first.
		if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: uuid + "-gpu", Name: "GPU", Vendor: "nvidia", MemoryTotal: 24 << 30, HealthStatus: unhealthyUnlessLeader(rank)}).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/gang"}); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node)
	}
	ids, _ := json.Marshal([]uint{nodes[0].ID, nodes[1].ID})
	dep, err := createTestDeployment(t, st, &store.Deployment{ModelID: model.ID, NodeID: nodes[0].ID, NodeIDs: string(ids), ServedName: "gang", Engine: "vllm", State: "active", NNodes: 2, MasterAddr: managementIP(*nodes[0]), MasterPort: 29500, Placement: `{"tp":2}`})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{nodes[0].UUID: true, nodes[1].UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if instances, err := st.InstancesForDeployment(dep.ID); err != nil || len(instances) != 0 {
		t.Fatalf("a replica whose follower does not fit was written: %+v %v", instances, err)
	}
	if len(hub.updates) != 0 {
		t.Fatalf("a refused replica was pushed: %+v", hub.updates)
	}
	event, found, err := st.LatestScheduleEvent(dep.ID)
	if err != nil || !found || event.Code != store.EventDeploymentUnschedulable {
		t.Fatalf("refusal = %+v found=%v err=%v", event, found, err)
	}
	var payload struct {
		Scheduler struct {
			Code string `json:"code"`
		} `json:"scheduler"`
		Rank *int   `json:"rank"`
		Node string `json:"node"`
	}
	if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
		t.Fatalf("refusal data %q: %v", event.Data, err)
	}
	if payload.Rank == nil || *payload.Rank != 1 || payload.Node != nodes[1].UUID || payload.Scheduler.Code == "" {
		t.Fatalf("refusal does not name the follower: %s", event.Data)
	}

	// Through Save, not a single-column Update: the card's derived columns
	// (its schedulable verdict) are filled by GPU.BeforeSave, which a
	// column update does not run.
	var followerCards []store.GPU
	if err := st.DB().Where("node_id = ?", nodes[1].ID).Find(&followerCards).Error; err != nil {
		t.Fatal(err)
	}
	for i := range followerCards {
		followerCards[i].HealthStatus = accelerator.HealthOK
		if err := st.DB().Save(&followerCards[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	instances, err := st.InstancesForDeployment(dep.ID)
	if err != nil || len(instances) != 2 {
		t.Fatalf("instances after the card came back = %+v %v, want both ranks", instances, err)
	}
	byRank := map[int]store.Instance{}
	for _, inst := range instances {
		byRank[inst.NodeRank] = inst
	}
	leader, follower := byRank[0], byRank[1]
	if leader.NodeID != nodes[0].ID || follower.NodeID != nodes[1].ID || !follower.Headless ||
		leader.DistGroup == "" || leader.DistGroup != follower.DistGroup || leader.DistAttempt != 1 || follower.DistAttempt != 1 {
		t.Fatalf("generation = leader %+v follower %+v", leader, follower)
	}
	for _, node := range nodes {
		if len(hub.updates[node.UUID]) == 0 {
			t.Fatalf("rank on %s was not pushed", node.UUID)
		}
	}
}

// Each multi-node replica is checked against the network its own nodes share.
// A node of another replica that cannot use that network (here a udocker node,
// which cannot expose RDMA devices) holds back only its own replica.
func TestAReplicasNetworkCheckCoversOnlyItsOwnNodes(t *testing.T) {
	st, err := store.Open("file:unit_gang_own_network?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "example/gang-rdma", Name: "gang-rdma", WeightBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"rdma-a", "rdma-b", "rdma-c", "rdma-d"}
	nodes := map[string]*store.Node{}
	online := map[string]bool{}
	var ids []uint
	for i, uuid := range names {
		node, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		ip := fmt.Sprintf("10.96.0.%d", i+1)
		inv := proto.NetInventory{
			Interfaces: []proto.NetInterface{{Name: "ib0", Addrs: []string{ip + "/24"}}, {Name: "eth0", Addrs: []string{fmt.Sprintf("10.4.0.%d/24", i+1)}}},
			RDMAPorts:  []proto.RDMAPort{{Device: "mlx5_0", Port: 1, State: "ACTIVE", LinkLayer: "InfiniBand", NetDev: "ib0"}},
			Management: &proto.ManagementAddress{Interface: "eth0", IP: fmt.Sprintf("10.4.0.%d", i+1)},
		}
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{Network: &inv}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetNodeLabels(uuid, map[string]string{"pool": "ib"}); err != nil {
			t.Fatal(err)
		}
		if uuid == "rdma-d" {
			if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Update("runtime_driver", "udocker").Error; err != nil {
				t.Fatal(err)
			}
		}
		if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: uuid + "-gpu", Name: "GPU", Vendor: "nvidia", MemoryTotal: 24 << 30}).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/gang-rdma"}); err != nil {
			t.Fatal(err)
		}
		nodes[uuid], online[uuid] = node, true
		ids = append(ids, node.ID)
	}
	ib, err := st.CreateNetwork(&store.Network{Name: "ib", Type: netplan.TypeHostRDMA, CIDR: "10.96.0.0/24", Roles: store.EncodeStringList([]string{netplan.RoleCompute})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateNetworkAttachment(&store.NetworkAttachment{NetworkID: ib.ID, LabelKey: "pool", LabelValue: "ib"}); err != nil {
		t.Fatal(err)
	}
	nodeIDs, _ := json.Marshal(ids)
	placement := `{"tp":2,"units":[{"node_uuid":"rdma-a","followers":[{"node_uuid":"rdma-b"}]},{"node_uuid":"rdma-c","followers":[{"node_uuid":"rdma-d"}]}]}`
	dep, err := createTestDeployment(t, st, &store.Deployment{ModelID: model.ID, NodeID: nodes["rdma-a"].ID, NodeIDs: string(nodeIDs), ServedName: "gang-rdma", Engine: "vllm", State: "active", NNodes: 2, MasterPort: 29500, Placement: placement})
	if err != nil {
		t.Fatal(err)
	}
	c := New(st, &distributedTestHub{online: online, updates: map[string][]proto.StateUpdate{}}, nil)
	// The second replica cannot launch: rdma-d runs udocker.
	_ = c.reconcileDeployment(context.Background(), dep.ID)
	instances, err := st.InstancesForDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	onNode := map[uint]bool{}
	for _, inst := range instances {
		onNode[inst.NodeID] = true
	}
	if len(instances) != 2 || !onNode[nodes["rdma-a"].ID] || !onNode[nodes["rdma-b"].ID] {
		t.Fatalf("instances = %+v, want only the first replica's two ranks", instances)
	}
}

// A push that fails for one rank does not stop the others: the generation is
// written, so a rank left unpushed would wait for a push nothing else makes.
// The failure is still reported.
func TestEveryRankIsPushedWhenOnePushFails(t *testing.T) {
	st, dep, nodes, hub := twoNodeGangFixture(t, "unit_gang_push")
	hub.pushFails = map[string]error{nodes[0].UUID: errors.New("node not connected")}
	err := New(st, hub, nil).reconcileDeployment(context.Background(), dep.ID)
	if err == nil || !strings.Contains(err.Error(), "node not connected") {
		t.Fatalf("reconcile error = %v, want the failed push", err)
	}
	if instances, _ := st.InstancesForDeployment(dep.ID); len(instances) != 2 {
		t.Fatalf("instances = %+v, want both ranks written", instances)
	}
	if len(hub.updates[nodes[1].UUID]) == 0 {
		t.Fatal("the follower was not pushed after the leader's push failed")
	}
}

// twoNodeGangFixture is a two-node, one-replica deployment whose nodes each
// have one free card and the model present.
func twoNodeGangFixture(t *testing.T, name string) (*store.Store, *store.Deployment, []*store.Node, *distributedTestHub) {
	t.Helper()
	st, err := store.Open("file:"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "example/" + name, Name: name, WeightBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	var nodes []*store.Node
	online := map[string]bool{}
	for rank := range 2 {
		uuid := fmt.Sprintf("%s-%d", name, rank)
		node, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		node.NetworkInventory = managementInventoryJSON(t, fmt.Sprintf("10.5.0.%d", rank+1))
		if err := st.DB().Save(node).Error; err != nil {
			t.Fatal(err)
		}
		if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: uuid + "-gpu", Name: "GPU", Vendor: "nvidia", MemoryTotal: 24 << 30}).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/" + name}); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node)
		online[uuid] = true
	}
	ids, _ := json.Marshal([]uint{nodes[0].ID, nodes[1].ID})
	dep, err := createTestDeployment(t, st, &store.Deployment{ModelID: model.ID, NodeID: nodes[0].ID, NodeIDs: string(ids), ServedName: name, Engine: "vllm", State: "active", NNodes: 2, MasterAddr: managementIP(*nodes[0]), MasterPort: 29500, Placement: `{"tp":2}`})
	if err != nil {
		t.Fatal(err)
	}
	return st, dep, nodes, &distributedTestHub{online: online, updates: map[string][]proto.StateUpdate{}}
}

// unhealthyUnlessLeader is the follower's card at a level that takes no
// work, so the gang cannot start until a later report clears it.
func unhealthyUnlessLeader(rank int) string {
	if rank == 0 {
		return accelerator.HealthOK
	}
	return accelerator.HealthError
}
