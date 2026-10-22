package dto

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/pkg/topoparse"
)

// storedTopology is nodes.topo_json as the server stores it: encoding/json
// of topoparse.Topology, PascalCase keys and integer link classes.
const storedTopology = `{"GPUs":[{"Index":0,"CPUAffinity":"0-23","NUMANode":0},{"Index":1,"CPUAffinity":"24-47","NUMANode":1}],` +
	`"Matrix":[[{"Raw":"X","Class":7,"NVLinkBonds":0},{"Raw":"NV12","Class":6,"NVLinkBonds":12}],` +
	`[{"Raw":"NV12","Class":6,"NVLinkBonds":12},{"Raw":"X","Class":7,"NVLinkBonds":0}]],` +
	`"FastCliques":[[0,1]],"NVLinkCliques":[[0,1]],"NUMAGroups":{"0":[0],"1":[1]}}`

func TestParseNodeTopologyAnswersStoredTopologyInSnakeCase(t *testing.T) {
	// The fixture must be what the store really holds.
	var check topoparse.Topology
	if err := json.Unmarshal([]byte(storedTopology), &check); err != nil {
		t.Fatal(err)
	}
	if again, _ := json.Marshal(check); string(again) != storedTopology {
		t.Fatalf("fixture is not topoparse's encoding:\n got %s\nwant %s", again, storedTopology)
	}

	raw, err := json.Marshal(ParseNodeTopology(storedTopology))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"gpus":[{"index":0,"cpu_affinity":"0-23","numa_node":0},{"index":1,"cpu_affinity":"24-47","numa_node":1}],` +
		`"matrix":[[{"raw":"X","class":7,"nvlink_bonds":0},{"raw":"NV12","class":6,"nvlink_bonds":12}],` +
		`[{"raw":"NV12","class":6,"nvlink_bonds":12},{"raw":"X","class":7,"nvlink_bonds":0}]],` +
		`"fast_cliques":[[0,1]],"nvlink_cliques":[[0,1]],"numa_groups":{"0":[0],"1":[1]}}`
	if string(raw) != want {
		t.Fatalf("topology wire JSON\n got %s\nwant %s", raw, want)
	}
}

// TestParseNodeTopologyBackfillsFastCliquesFromOldRows: rows written before
// FastCliques existed carry only NVLinkCliques; topoparse backfills it and
// the wire form shows both.
func TestParseNodeTopologyBackfillsFastCliquesFromOldRows(t *testing.T) {
	old := `{"GPUs":[{"Index":0,"CPUAffinity":"","NUMANode":-1}],"Matrix":[[{"Raw":"X","Class":7,"NVLinkBonds":0}]],"NVLinkCliques":[[0,1],[2,3]],"NUMAGroups":null}`
	raw, err := json.Marshal(ParseNodeTopology(old))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"gpus":[{"index":0,"cpu_affinity":"","numa_node":-1}],"matrix":[[{"raw":"X","class":7,"nvlink_bonds":0}]],` +
		`"fast_cliques":[[0,1],[2,3]],"nvlink_cliques":[[0,1],[2,3]],"numa_groups":null}`
	if string(raw) != want {
		t.Fatalf("old topology wire JSON\n got %s\nwant %s", raw, want)
	}
}

func TestParseNodeTopologyEmptyMatrixIsEmptyArrays(t *testing.T) {
	raw, err := json.Marshal(ParseNodeTopology(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"gpus":[],"matrix":[],"fast_cliques":null,"nvlink_cliques":null,"numa_groups":null}` {
		t.Fatalf("empty topology = %s", raw)
	}
}

func TestParseNodeTopologyUnparseableIsNil(t *testing.T) {
	for _, raw := range []string{"", "not json", `{"GPUs":"x"}`, `[1,2]`, `{"Matrix":[[{"Class":"NV"}]]}`} {
		if got := ParseNodeTopology(raw); got != nil {
			t.Fatalf("ParseNodeTopology(%q) = %+v, want nil", raw, got)
		}
	}
}
