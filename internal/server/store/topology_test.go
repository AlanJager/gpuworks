package store

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/pkg/topoparse"
)

func TestParseTopoJSONAcceptsAscendHCCS(t *testing.T) {
	raw := "\tNPU2\tNPU4\tCPU Affinity\tNUMA Affinity\n" +
		"NPU2\t X \tHCCS\t0-31\t0\n" +
		"NPU4\tHCCS\t X \t0-31\t0\n"
	js := parseTopoJSON(raw)
	if js == "" {
		t.Fatal("parseTopoJSON returned empty")
	}
	var topo topoparse.Topology
	if err := json.Unmarshal([]byte(js), &topo); err != nil {
		t.Fatal(err)
	}
	if len(topo.GPUs) != 2 || topo.GPUs[0].Index != 2 || topo.GPUs[1].Index != 4 {
		t.Fatalf("GPUs = %+v", topo.GPUs)
	}
	if len(topo.FastCliques) != 1 || len(topo.FastCliques[0]) != 2 {
		t.Fatalf("FastCliques = %+v", topo.FastCliques)
	}
}
