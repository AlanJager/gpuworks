package api

import (
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"net/http"
	"reflect"
	"testing"
)

func TestSearchNetworkTargets(t *testing.T) {
	n := newNetworkAPI(t)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks", `{"name":"fabric","roles":["compute"],"type":"host_rdma"}`, http.StatusCreated)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks/1/attachments", `{"label_key":"pool","label_value":"Training"}`, http.StatusCreated)
	for _, tc := range []struct {
		kind, q string
		want    []any
	}{
		{"attachments", "POOL%3Dtrain", []any{"1"}},
		{"nodes", "train", []any{}},
		{"attachments", "absent", []any{}},
	} {
		got := n.expect(n.member, http.MethodGet, "/api/v1/networks/1/search?kind="+tc.kind+"&q="+tc.q, "", http.StatusOK)["ids"]
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s %s: got %v want %v", tc.kind, tc.q, got, tc.want)
		}
	}
	n.expect(n.member, http.MethodGet, "/api/v1/networks/999/search?kind=nodes", "", http.StatusNotFound)
	n.expect(n.member, http.MethodGet, "/api/v1/networks/1/search?kind=unknown", "", http.StatusBadRequest)
}

func TestNetworkTargetSearchFields(t *testing.T) {
	detail := dto.NetworkDetail{Network: dto.Network{Attachments: []dto.NetworkAttachment{{ID: 2, Target: "node", NodeName: "worker", HCAs: []string{"mlx5_0:1"}}}}, Nodes: []dto.NetworkNode{
		{NodeUUID: "abc", NodeName: "worker", IP: "192.0.2.4", HCAs: []dto.NetworkRail{{Name: "mlx5_0:1", IP: "198.51.100.2", NetDev: "ens3"}}, Labels: map[string]string{"zone": "East"}},
	}}
	for _, q := range []string{"WORK", "192.0.2.", "mlx5", "198.51.100.", "ENS3", "zone=east"} {
		if ids := matchNetworkTargets(detail, "nodes", q).IDs; !reflect.DeepEqual(ids, []string{"abc"}) {
			t.Errorf("%q: %v", q, ids)
		}
	}
	if ids := matchNetworkTargets(detail, "attachments", "=").IDs; len(ids) != 0 {
		t.Fatalf("node matched empty label: %v", ids)
	}
}
