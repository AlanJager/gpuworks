package api

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
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
	if _, err := n.store.RegisterNode("gpu-direct", store.HashToken("local"), "gpu-direct"); err != nil {
		t.Fatal(err)
	}
	n.expect(n.admin, http.MethodPost, "/api/v1/networks/1/attachments", `{"node_uuid":"gpu-direct"}`, http.StatusCreated)
	for _, tc := range []struct {
		params string
		want   []any
	}{
		{"role=serving&role=compute&source=label&source=node", []any{"gpu-direct"}},
		{"role=serving&source=node", []any{}},
		{"role=compute&source=label", []any{}},
	} {
		got := n.expect(n.member, http.MethodGet, "/api/v1/networks/1/search?kind=nodes&q=gpu&"+tc.params, "", http.StatusOK)["ids"]
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v want %v", tc.params, got, tc.want)
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
		if ids := matchNetworkTargets(detail, "nodes", q, nil, nil).IDs; !reflect.DeepEqual(ids, []string{"abc"}) {
			t.Errorf("%q: %v", q, ids)
		}
	}
	if ids := matchNetworkTargets(detail, "attachments", "=", nil, nil).IDs; len(ids) != 0 {
		t.Fatalf("node matched empty label: %v", ids)
	}
}

func TestNetworkNodeSearchIncludesOnlyItsOwnOverrides(t *testing.T) {
	detail := dto.NetworkDetail{
		Network: dto.Network{Attachments: []dto.NetworkAttachment{
			{ID: 2, Target: "node", Interface: "bond7", IP: "192.0.2.101", HCAs: []string{"absent_rail:9"}},
			{ID: 3, Target: "node", Interface: "unresolved-only"},
		}},
		Nodes: []dto.NetworkNode{
			{NodeUUID: "node-a", AttachmentID: 2, Interface: "eth0"},
			{NodeUUID: "node-b", AttachmentID: 4, Interface: "eth1"},
		},
	}
	for _, q := range []string{"ETH0", "bond7", "192.0.2.101", "ABSENT_RAIL"} {
		if ids := matchNetworkTargets(detail, "nodes", q, nil, nil).IDs; !reflect.DeepEqual(ids, []string{"node-a"}) {
			t.Errorf("search %q: %v", q, ids)
		}
	}
	if ids := matchNetworkTargets(detail, "nodes", "unresolved-only", nil, nil).IDs; len(ids) != 0 {
		t.Errorf("unresolved override matched resolved nodes: %v", ids)
	}
}

func TestNetworkNodeSearchCombinesRoleSourceAndQuery(t *testing.T) {
	detail := dto.NetworkDetail{Nodes: []dto.NetworkNode{
		{NodeUUID: "a", NodeName: "gpu-01", Roles: []string{"serving", "compute"}, Source: "label"},
		{NodeUUID: "b", NodeName: "gpu-02", Roles: []string{"storage_sync"}, Source: "label"},
		{NodeUUID: "c", NodeName: "gpu-03", Roles: []string{"compute"}, Source: "node"},
		{NodeUUID: "d", NodeName: "other", Roles: []string{"compute"}, Source: "label"},
	}}
	for _, tc := range []struct {
		q                    string
		roles, sources, want []string
	}{
		{"gpu", []string{"compute", "storage_sync"}, []string{"label"}, []string{"a", "b"}},
		{"gpu", []string{"compute"}, []string{"label", "node"}, []string{"a", "c"}},
		{"", []string{"serving"}, nil, []string{"a"}},
		{"", nil, []string{"node"}, []string{"c"}},
		{"other", nil, []string{"node"}, []string{}},
	} {
		if got := matchNetworkTargets(detail, "nodes", tc.q, tc.roles, tc.sources).IDs; !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%+v: got %v", tc, got)
		}
	}
}

func TestNetworkSearchScopeAtFleetScale(t *testing.T) {
	// Keep the five-second cache window independent of runner speed.
	synctest.Test(t, testNetworkSearchScopeAtFleetScale)
}

func testNetworkSearchScopeAtFleetScale(t *testing.T) {
	n := newNetworkAPI(t)
	nodes := make([]store.Node, 1000)
	for i := range nodes {
		nodes[i].UUID = fmt.Sprintf("node-%04d", i)
		nodes[i].Name = nodes[i].UUID
		nodes[i].NetworkInventory = `{"management":{"interface":"eth0","ip":"192.0.2.1"}}`
	}
	nodes[0].Labels = `{"pool":"east"}`
	nodes[1].Labels = nodes[0].Labels
	nodes[2].Name = "CAFÉ"
	if err := n.store.DB().CreateInBatches(&nodes, 10).Error; err != nil {
		t.Fatal(err)
	}
	// An earlier direct rule on the competitor wins for node 0. node 1
	// remains on fabric, even though its membership comes from the same label.
	for _, name := range []string{"competitor", "fabric", "unrelated"} {
		n.expect(n.admin, http.MethodPost, "/api/v1/networks", fmt.Sprintf(`{"name":%q,"type":"host_ethernet","roles":["compute"]}`, name), http.StatusCreated)
	}
	n.expect(n.admin, http.MethodPost, "/api/v1/networks/1/attachments", `{"node_uuid":"node-0000"}`, http.StatusCreated)
	n.expect(n.admin, http.MethodPost, "/api/v1/networks/2/attachments", `{"label_key":"pool","label_value":"east"}`, http.StatusCreated)
	attachments := make([]store.NetworkAttachment, 0, len(nodes)-2)
	for _, node := range nodes[2:] {
		attachments = append(attachments, store.NetworkAttachment{NetworkID: 3, NodeID: node.ID})
	}
	if err := n.store.DB().CreateInBatches(&attachments, 50).Error; err != nil {
		t.Fatal(err)
	}

	reads := map[string]int64{}
	hook := "test:network_search_scope"
	if err := n.store.DB().Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
		reads[tx.Statement.Table] += tx.RowsAffected
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.store.DB().Callback().Query().Remove(hook) })
	got := n.expect(n.member, http.MethodGet, "/api/v1/networks/2/search?kind=nodes&q=pool", "", http.StatusOK)["ids"]
	if !reflect.DeepEqual(got, []any{"node-0001"}) {
		t.Fatalf("winner after competing assignment: %v", got)
	}
	if reads["nodes"] != 1000 || reads["gpus"] != 0 {
		t.Fatalf("cold search should build one fleet snapshot: %v", reads)
	}
	clear(reads)
	got = n.expect(n.member, http.MethodGet, "/api/v1/networks/2/search?kind=attachments&q=pool", "", http.StatusOK)["ids"]
	if !reflect.DeepEqual(got, []any{"2"}) || reads["nodes"] != 0 || reads["network_attachments"] != 0 || reads["networks"] != 0 {
		t.Fatalf("searching one label attachment: result %v, reads %v", got, reads)
	}
	// Opening the detail page primes the same snapshot used by typing and
	// changing filters, including on the thousand-node management network.
	n.expect(n.member, http.MethodGet, "/api/v1/networks/0", "", http.StatusOK)
	for _, tc := range []struct {
		query string
		want  []any
	}{
		{"q=node-0999", []any{"node-0999"}},
		{"q=caf%C3%A9", []any{"node-0002"}},
		{"q=POOL%3DEAST", []any{"node-0000", "node-0001"}},
		{"q=node-0999&role=compute", []any{}},
		{"q=node-0999&role=serving", []any{"node-0999"}},
	} {
		clear(reads)
		got = n.expect(n.member, http.MethodGet, "/api/v1/networks/0/search?kind=nodes&"+tc.query, "", http.StatusOK)["ids"]
		if !reflect.DeepEqual(got, tc.want) || reads["nodes"] != 0 || reads["network_attachments"] != 0 || reads["networks"] != 0 {
			t.Errorf("management %s: got %v, want %v; reads %v", tc.query, got, tc.want, reads)
		}
	}
	clear(reads)
	got = n.expect(n.member, http.MethodGet, "/api/v1/networks/2/search?kind=nodes&q=pool", "", http.StatusOK)["ids"]
	if !reflect.DeepEqual(got, []any{"node-0001"}) || reads["nodes"] != 0 || reads["networks"] != 0 {
		t.Errorf("switching back to fabric: %v, reads %v", got, reads)
	}

}

// A database write on another member has no local bus event. An explicit
// search rebuild after expiry must bypass the underlying fleet snapshot.
func TestNetworkSearchUnannouncedWrites(t *testing.T) {
	// Keep the five-second cache window independent of runner speed.
	synctest.Test(t, testNetworkSearchUnannouncedWrites)
}

func testNetworkSearchUnannouncedWrites(t *testing.T) {
	n := newNetworkAPI(t)
	node := store.Node{UUID: "u1", Name: "old", NetworkInventory: `{"management":{"interface":"eth0","ip":"192.0.2.1"}}`}
	if err := n.store.DB().Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	a := apiHandlers{d: Deps{Store: n.store}}
	assertName := func(want string) {
		t.Helper()
		view, err := a.searchableNetwork(0)
		if err != nil || len(view.Nodes) != 1 || view.Nodes[0].NodeName != want {
			t.Fatalf("expected %q, got %+v, %v", want, view.Nodes, err)
		}
	}
	rename := func(name string) {
		t.Helper()
		if err := n.store.DB().Model(&node).Update("name", name).Error; err != nil {
			t.Fatal(err)
		}
	}
	n.store.EnableFleetNodeSnapshot(nil)
	if _, err := n.store.FleetNodes(); err != nil {
		t.Fatal(err)
	}
	assertName("old")
	rename("expired")
	time.Sleep(networkSearchMaxAge - time.Nanosecond)
	assertName("old")
	time.Sleep(time.Nanosecond)
	assertName("expired")
}

func TestNetworkSearchSeesChangedFactsAndReusesRoutineReports(t *testing.T) {
	n := newNetworkAPI(t)
	node := store.Node{UUID: "u1"}
	if err := n.store.DB().Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	report := proto.NodeStatus{Sys: proto.SysInfo{Hostname: "old-host"}, Network: &proto.NetInventory{Management: &proto.ManagementAddress{IP: "192.0.2.1", Interface: "eth0"}}}
	send := func() {
		t.Helper()
		if err := n.store.UpdateNodeStatus("u1", report); err != nil {
			t.Fatal(err)
		}
	}
	send()
	a := apiHandlers{d: Deps{Store: n.store}}
	check := func(name, ip string) {
		t.Helper()
		view, err := a.searchableNetwork(0)
		if err != nil || len(view.Nodes) != 1 || view.Nodes[0].NodeName != name || view.Nodes[0].IP != ip {
			t.Fatalf("want %s %s, got %+v, err %v", name, ip, view.Nodes, err)
		}
	}
	check("old-host", "192.0.2.1")
	started := a.networkSearch.entries[0].started
	for range 3 {
		send()
		check("old-host", "192.0.2.1")
	}
	if a.networkSearch.entries[0].started != started {
		t.Fatal("ordinary unchanged reports invalidated search")
	}
	// A normal overview can still see the previous hostname inside the fleet
	// report window. It must not seed search with those rows as the new generation.
	n.store.EnableFleetNodeSnapshot(func() time.Time { return time.Unix(100, 0) })
	if _, err := n.store.FleetNodes(); err != nil {
		t.Fatal(err)
	}
	report.Sys.Hostname = "new-host"
	send()
	n.expect(n.member, http.MethodGet, "/api/v1/networks/0", "", http.StatusOK)
	got := n.expect(n.member, http.MethodGet, "/api/v1/networks/0/search?kind=nodes&q=new-host", "", http.StatusOK)
	if !reflect.DeepEqual(got["ids"], []any{"u1"}) {
		t.Fatalf("search after overview returned %v", got)
	}
	check("new-host", "192.0.2.1")
	report.Network.Management.IP = "192.0.2.2"
	send()
	check("new-host", "192.0.2.2")
}

func TestNetworkSearchCacheEvictsOldestBuild(t *testing.T) {
	var c networkSearchCache
	start := time.Now()
	for i := range networkSearchCapacity + 1 {
		c.remember(uint(i), networkSearchEntry{started: start.Add(time.Duration(i) * time.Second)})
	}
	if len(c.entries) != networkSearchCapacity {
		t.Fatalf("retained %d networks", len(c.entries))
	}
	if _, ok := c.entries[0]; ok {
		t.Fatal("oldest network not evicted")
	}
	older := networkSearchEntry{started: start}
	c.remember(1, older)
	if !c.entries[1].started.After(older.started) {
		t.Fatal("older build replaced newer entry")
	}
}
