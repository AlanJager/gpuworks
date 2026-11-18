package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRoCEAddressWirePreservesLegacyPhysicalAssociation(t *testing.T) {
	const legacy = `{"device":"mlx5_0","port":1,"netdev":"ens3f0"}`
	var port RDMAPort
	if err := json.Unmarshal([]byte(legacy), &port); err != nil {
		t.Fatal(err)
	}
	if port.RoCEAddress != nil {
		t.Fatal("an older report invented a GID address")
	}
	raw, err := json.Marshal(port)
	if err != nil || string(raw) != legacy {
		t.Fatalf("legacy wire changed: %s, %v", raw, err)
	}
	port.RoCEAddress = &RoCEAddress{NetDev: "bond0", CIDR: "192.0.2.12/24"}
	raw, err = json.Marshal(port)
	if err != nil || !strings.Contains(string(raw), `"roce_address":{"netdev":"bond0","cidr":"192.0.2.12/24"}`) {
		t.Fatalf("missing typed GID address: %s, %v", raw, err)
	}
	var olderReader struct {
		NetDev string `json:"netdev"`
	}
	if err := json.Unmarshal(raw, &olderReader); err != nil || olderReader.NetDev != "ens3f0" {
		t.Fatalf("older reader lost physical association: %+v, %v", olderReader, err)
	}
}
