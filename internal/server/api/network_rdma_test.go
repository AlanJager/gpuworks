package api

import (
	"net/http"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// rdmaNodes registers two RoCE nodes shaped like the Ascend lab hosts: two
// 25G rails with addresses and a 1G port without one.
func rdmaNodes(t *testing.T, n *networkAPI) {
	t.Helper()
	for i, uuid := range []string{"asc-1", "asc-2"} {
		if _, err := n.store.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
		last := []string{"209", "210"}[i]
		inv := proto.NetInventory{
			Interfaces: []proto.NetInterface{
				{Name: "enp61s0f0", SpeedMbps: 25000, Addrs: []string{"100.64.29." + last + "/24"}},
				{Name: "enp61s0f2", SpeedMbps: 25000, Addrs: []string{"100.64.30." + last + "/24"}},
				{Name: "enp189s0f0", SpeedMbps: 1000},
			},
			RDMAPorts: []proto.RDMAPort{
				{Device: "rocep189s0f0", Port: 1, State: "ACTIVE", LinkLayer: "Ethernet", RateGbps: 2.5, NetDev: "enp189s0f0"},
				{Device: "rocep61s0f0", Port: 1, State: "ACTIVE", LinkLayer: "Ethernet", RateGbps: 25, NetDev: "enp61s0f0"},
				{Device: "rocep61s0f2", Port: 1, State: "ACTIVE", LinkLayer: "Ethernet", RateGbps: 25, NetDev: "enp61s0f2"},
			},
			Tools: []string{"ib_write_bw"},
		}
		if err := n.store.UpdateNodeStatus(uuid, proto.NodeStatus{Network: &inv}); err != nil {
			t.Fatal(err)
		}
	}
}

// nodeOf picks one node out of a network detail.
func nodeOf(t *testing.T, detail map[string]any, uuid string) map[string]any {
	t.Helper()
	for _, raw := range detail["nodes"].([]any) {
		if node := raw.(map[string]any); node["node_uuid"] == uuid {
			return node
		}
	}
	t.Fatalf("%s not in %v", uuid, detail["nodes"])
	return nil
}

func railNamesOf(node map[string]any) []string {
	var out []string
	for _, r := range node["hcas"].([]any) {
		out = append(out, r.(map[string]any)["name"].(string))
	}
	return out
}

func TestNetworkAttachBatch(t *testing.T) {
	n := newNetworkAPI(t)
	rdmaNodes(t, n)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"f","roles":["compute"],"type":"host_rdma"}`, http.StatusCreated)
	batch := "/api/v1/networks/1/attachments/batch"

	n.expect(n.member, http.MethodPost, batch, `{"node_uuids":["asc-1"]}`, http.StatusForbidden)
	n.expect(n.admin, http.MethodGet, batch, "", http.StatusMethodNotAllowed)
	n.expect(n.admin, http.MethodPost, batch, `{`, http.StatusBadRequest)
	n.expectCode(http.MethodPost, batch, `{"node_uuids":[]}`, http.StatusBadRequest, ErrorNetworkAttachmentTargetInvalid)
	n.expectCode(http.MethodPost, "/api/v1/networks/9/attachments/batch", `{"node_uuids":["asc-1"]}`, http.StatusNotFound, ErrorNetworkNotFound)

	out := n.expect(n.admin, http.MethodPost, batch, `{"node_uuids":["asc-1","asc-2","gone"]}`, http.StatusOK)
	if !reflect.DeepEqual(keys(out), []string{"network", "results"}) {
		t.Fatalf("keys = %v", keys(out))
	}
	wantResults := []any{
		map[string]any{"node_uuid": "asc-1", "status": "attached"},
		map[string]any{"node_uuid": "asc-2", "status": "attached"},
		map[string]any{"node_uuid": "gone", "status": "failed", "code": ErrorNetworkAttachmentTargetInvalid, "params": map[string]any{"node": "gone"},
			"error": "invalid network target: attachment node not found"},
	}
	if !reflect.DeepEqual(out["results"], wantResults) {
		t.Fatalf("results = %v", out["results"])
	}
	nodes := out["network"].(map[string]any)["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("nodes = %v", nodes)
	}
	// Nothing configured: the 25G rails, their addresses, the first rail's
	// address as bootstrap.
	first := nodeOf(t, out["network"].(map[string]any), "asc-1")
	wantRail := map[string]any{"name": "rocep61s0f0:1", "ip": "100.64.29.209", "netdev": "enp61s0f0", "link_layer": "roce", "state": "ACTIVE", "speed_gbps": float64(25)}
	if !reflect.DeepEqual(railNamesOf(first), []string{"rocep61s0f0:1", "rocep61s0f2:1"}) || !reflect.DeepEqual(first["hcas"].([]any)[0], wantRail) ||
		first["ip"] != "100.64.29.209" || first["interface"] != "enp61s0f0" || first["hca_source"] != "auto" || first["bootstrap_source"] != "auto" {
		t.Fatalf("node = %v", first)
	}

	// A retry is harmless.
	out = n.expect(n.admin, http.MethodPost, batch, `{"node_uuids":["asc-1"]}`, http.StatusOK)
	if out["results"].([]any)[0].(map[string]any)["status"] != "already" {
		t.Fatalf("retry = %v", out["results"])
	}
}

func TestNetworkHCAEdits(t *testing.T) {
	n := newNetworkAPI(t)
	rdmaNodes(t, n)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"f","roles":["compute"],"type":"host_rdma"}`, http.StatusCreated)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"lan","roles":["storage_sync"],"type":"host_ethernet"}`, http.StatusCreated)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks/1/attachments/batch", `{"node_uuids":["asc-1","asc-2"]}`, http.StatusOK)
	hcas := "/api/v1/networks/1/hcas"

	n.expect(n.member, http.MethodPost, hcas, `{"op":"reset","node_uuid":"asc-1"}`, http.StatusForbidden)
	n.expect(n.admin, http.MethodGet, hcas, "", http.StatusMethodNotAllowed)
	n.expect(n.admin, http.MethodPost, hcas, `{`, http.StatusBadRequest)
	n.expectCode(http.MethodPost, "/api/v1/networks/9/hcas", `{"op":"reset","node_uuid":"asc-1"}`, http.StatusNotFound, ErrorNetworkNotFound)
	n.expectCode(http.MethodPost, "/api/v1/networks/2/hcas", `{"op":"reset","node_uuid":"asc-1"}`, http.StatusBadRequest, ErrorNetworkHCANotRDMA)
	n.expectCode(http.MethodPost, hcas, `{"op":"swap"}`, http.StatusBadRequest, ErrorNetworkOverrideInvalid)
	n.expectCode(http.MethodPost, hcas, `{"op":"add","node_uuid":"nope","hca":"rocep61s0f0"}`, http.StatusBadRequest, ErrorNetworkHCANodeInvalid)
	n.expectCode(http.MethodPost, hcas, `{"op":"add","node_uuid":"asc-1","hca":"mlx5_0"}`, http.StatusBadRequest, ErrorNetworkHCAInvalid)
	n.expectCode(http.MethodPost, hcas, `{"op":"remove","hca":"rocep189s0f0:1"}`, http.StatusBadRequest, ErrorNetworkHCAInvalid)

	// Add the 1G port to one node: it is pinned, and the port is flagged.
	detail := n.expect(n.admin, http.MethodPost, hcas, `{"op":"add","node_uuid":"asc-1","hca":"rocep189s0f0"}`, http.StatusOK)
	one := nodeOf(t, detail, "asc-1")
	wantWarnings := []any{
		map[string]any{"code": "hca_unusable", "params": map[string]any{"value": "rocep189s0f0:1"}},
		map[string]any{"code": "hca_mismatch", "params": map[string]any{"count": "3", "counts": "2, 3"}},
	}
	if !reflect.DeepEqual(railNamesOf(one), []string{"rocep61s0f0:1", "rocep61s0f2:1", "rocep189s0f0:1"}) || one["hca_source"] != "manual" || !reflect.DeepEqual(one["warnings"], wantWarnings) {
		t.Fatalf("after add = %v", one)
	}
	if gap := one["hcas"].([]any)[2].(map[string]any)["gap"]; gap != "no_ipv4" {
		t.Fatalf("1G port gap = %v", gap)
	}

	// Remove a rail from every node.
	detail = n.expect(n.admin, http.MethodPost, hcas, `{"op":"remove","hca":"rocep61s0f2:1"}`, http.StatusOK)
	for _, raw := range detail["nodes"].([]any) {
		node := raw.(map[string]any)
		if names := railNamesOf(node); len(names) == 0 || names[0] != "rocep61s0f0:1" || contains(names, "rocep61s0f2:1") {
			t.Fatalf("after remove = %v", node)
		}
	}
	n.expectCode(http.MethodPost, hcas, `{"op":"remove","hca":"rocep61s0f0:1","node_uuid":"asc-2"}`, http.StatusConflict, ErrorNetworkHCALast)

	detail = n.expect(n.admin, http.MethodPost, hcas, `{"op":"reset","node_uuid":"asc-1"}`, http.StatusOK)
	if one := nodeOf(t, detail, "asc-1"); one["hca_source"] != "auto" || len(railNamesOf(one)) != 2 {
		t.Fatalf("after reset = %v", one)
	}

	// The node page lists every port with its address and gap, for adding one.
	view := n.expect(n.member, http.MethodGet, "/api/v1/nodes/asc-1/networks", "", http.StatusOK)
	port := view["rdma_ports"].([]any)[0].(map[string]any)
	if port["device"] != "rocep189s0f0" || port["ip"] != "" || port["gap"] != "no_ipv4" {
		t.Fatalf("port = %v", port)
	}
	if loaded := view["networks"].([]any)[0].(map[string]any); loaded["hca_source"] != "auto" || loaded["bootstrap_source"] != "auto" {
		t.Fatalf("loaded = %v", loaded)
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
