package store

import "testing"

// RoutingNodes reads every node's identity and runtime driver in one query; a
// node that has not reported a driver yet is absent from drivers.
func TestRoutingNodesReadsIdentityAndRuntimeDriver(t *testing.T) {
	st := newTestStore(t)
	mac, err := st.RegisterNode("mac", HashToken("mac"), "mac")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := st.RegisterNode("fresh", HashToken("fresh"), "fresh")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&Node{}).Where("id = ?", mac.ID).Update("runtime_driver", "host").Error; err != nil {
		t.Fatal(err)
	}
	uuids, drivers, err := st.RoutingNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(uuids) != 2 || uuids[mac.ID] != mac.UUID || uuids[fresh.ID] != fresh.UUID {
		t.Fatalf("uuids = %v", uuids)
	}
	if len(drivers) != 1 || drivers[mac.ID] != "host" {
		t.Fatalf("drivers = %v", drivers)
	}
}
