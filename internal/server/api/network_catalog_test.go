package api

import (
	"net/http"
	"strconv"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Without dedicated networks the pages still list the management network, but
// every change to declared networks is refused with a stable code; the
// management network's per-node roles stay editable.
func TestNetworkChangesWithoutDedicatedNetworks(t *testing.T) {
	n := newNetworkAPI(t)
	n.router = Router(Deps{Store: n.store, AuthSigner: n.signer, Networks: network.New(n.store, network.ManagementOnly{})})
	// A row left from before, for instance: it must neither show nor apply.
	existing, err := n.store.CreateNetwork(&store.Network{Name: "old", Type: netplan.TypeHostEthernet, Roles: store.EncodeStringList([]string{netplan.RoleStorageSync})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.store.RegisterNode("mgmt-only", store.HashToken("t"), "mgmt-only"); err != nil {
		t.Fatal(err)
	}
	list := n.expect(n.member, http.MethodGet, "/api/v1/networks", "", http.StatusOK)
	if items := list["items"].([]any); len(items) != 1 || items[0].(map[string]any)["builtin"] != true {
		t.Fatalf("items = %v", items)
	}
	const unavailable = ErrorNetworkDedicatedUnavailable
	n.expectCode(http.MethodPost, "/api/v1/networks", `{"name":"ib","roles":["compute"],"type":"host_rdma"}`, http.StatusForbidden, unavailable)
	n.expectCode(http.MethodPut, "/api/v1/networks/"+strconv.FormatUint(uint64(existing.ID), 10), `{"name":"old","roles":["compute"],"type":"host_ethernet"}`, http.StatusForbidden, unavailable)
	n.expectCode(http.MethodDelete, "/api/v1/networks/"+strconv.FormatUint(uint64(existing.ID), 10), "", http.StatusForbidden, unavailable)
	n.expectCode(http.MethodPost, "/api/v1/networks/"+strconv.FormatUint(uint64(existing.ID), 10)+"/attachments", `{"label_key":"pool","label_value":"a"}`, http.StatusForbidden, unavailable)
	view := n.expect(n.admin, http.MethodPut, "/api/v1/nodes/mgmt-only/management-roles", `{"roles_off":["compute"]}`, http.StatusOK)
	if got := view["management"].(map[string]any)["roles_off"].([]any); len(got) != 1 || got[0] != "compute" {
		t.Fatalf("roles_off = %v", got)
	}
}
