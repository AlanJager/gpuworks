package dto

import (
	"encoding/json"
	"testing"
)

// A storage carries its identity, type, switches, where it lives, which nodes
// it applies to and a node summary; targets and problems are always arrays.
func TestStorageWireContract(t *testing.T) {
	const wire = `{"id":2,"name":"model-archive","type":"nfs","enabled":true,"is_default":false,"all_nodes":true,"path":"/mnt/gpuworks/model-archive","export":"nfs.example.internal:/export/models","mount_options":"ro","total_bytes":128000,"available_bytes":52000,"targets":[{"node_uuid":"n1","node_name":"gpu3","path":"/srv/models","existing":true}],"nodes":{"total":130,"ready":128,"problems":[{"node_uuid":"n7","node_name":"node-07","state":"unresponsive","reason":"other","writable":false}]},"writable":true,"model_count":480,"deployment_ids":[3]}`
	var decoded Storage
	if err := json.Unmarshal([]byte(wire), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Type != "nfs" || decoded.Nodes.Ready != 128 || decoded.Nodes.Problems[0].State != "unresponsive" || !decoded.Targets[0].Existing {
		t.Fatalf("decoded = %+v", decoded)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != wire {
		t.Fatalf("round trip = %s\nwant        %s", encoded, wire)
	}
	encoded, err = json.Marshal(Storage{ID: 1, Name: "default", Type: "local", Targets: []StorageTarget{}, Nodes: StorageNodes{Problems: []StorageNodeStatus{}}, DeploymentIDs: []uint{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"id":1,"name":"default","type":"local","enabled":false,"is_default":false,"all_nodes":false,"path":"","targets":[],"nodes":{"total":0,"ready":0,"problems":[]},"writable":false,"model_count":0,"deployment_ids":[]}` {
		t.Fatalf("local storage = %s", encoded)
	}
}
