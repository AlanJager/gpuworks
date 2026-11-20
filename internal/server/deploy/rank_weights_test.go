package deploy

import (
	"encoding/json"
	"testing"
)

func TestRankSubfolderExpansionAndMatching(t *testing.T) {
	for _, template := range []string{"rank${NODE_RANK}", "rank${GPUWORKS_NODE_RANK}"} {
		if !RankScopedSubfolder(template) {
			t.Fatalf("%s is not rank-scoped", template)
		}
		if got := ExpandRankSubfolder(template, 3); got != "rank3" {
			t.Fatalf("expand %s = %q", template, got)
		}
		if !RankSubfolderMatches(template, "rank12") {
			t.Fatalf("%s does not match rank12", template)
		}
		for _, other := range []string{"", "rank", "rank1/x", "x/rank1", "rankA", "rank${NODE_RANK}"} {
			if RankSubfolderMatches(template, other) {
				t.Fatalf("%s matches %q", template, other)
			}
		}
		if rank, ok := RankOfSubfolder(template, "rank07"); ok {
			t.Fatalf("rank07 is no rank's expansion, got %d", rank)
		}
	}
	nested := "tp4/r${NODE_RANK}/part${NODE_RANK}"
	if rank, ok := RankOfSubfolder(nested, "tp4/r2/part2"); !ok || rank != 2 {
		t.Fatalf("nested = %d %v", rank, ok)
	}
	if _, ok := RankOfSubfolder(nested, "tp4/r2/part3"); ok {
		t.Fatal("a subfolder naming two ranks matched")
	}
	if RankScopedSubfolder("rank0") || ExpandRankSubfolder("rank0", 2) != "rank0" || !RankSubfolderMatches("rank0", "rank0") {
		t.Fatal("a plain subfolder is not left as it is")
	}
	if got := UnknownSubfolderVariable("rank${NODE_RANK}/${GPU_COUNT}"); got != "${GPU_COUNT}" {
		t.Fatalf("unknown variable = %q", got)
	}
	if got := UnknownSubfolderVariable("rank${GPUWORKS_NODE_RANK}"); got != "" {
		t.Fatalf("the prefixed rank variable was refused: %q", got)
	}
	// Both spellings are one variable under the launch fields' alias rule
	// (placement.Canonical), so they may be mixed and still name one rank;
	// a lower-case or prefixed non-rank name is refused like any other.
	mixed := "r${GPUWORKS_NODE_RANK}/part${NODE_RANK}"
	if got := ExpandRankSubfolder(mixed, 1); got != "r1/part1" {
		t.Fatalf("mixed expansion = %q", got)
	}
	if rank, ok := RankOfSubfolder(mixed, "r1/part1"); !ok || rank != 1 {
		t.Fatalf("mixed match = %d %v", rank, ok)
	}
	for _, other := range []string{"rank${node_rank}", "rank${GPUWORKS_NNODES}", "rank${GPUWORKS_}"} {
		if RankScopedSubfolder(other) || UnknownSubfolderVariable(other) == "" {
			t.Fatalf("%s is accepted as the rank", other)
		}
	}
}

// The binding is a stored contract: exact wire keys, round-tripped through
// the snapshot, and an old snapshot without it reads as unscoped.
func TestRankWeightsSnapshotWire(t *testing.T) {
	selection := CatalogSelection{ModelID: "m", Engine: "callable", WeightSubfolder: "rank${NODE_RANK}",
		RankWeights: []RankWeight{{Rank: 0, Subfolder: "rank0", StoreModelID: 4}, {Rank: 1, Subfolder: "rank1", StoreModelID: 9}}}
	encoded, err := selection.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if got := string(wire["rank_weights"]); got != `[{"rank":0,"subfolder":"rank0","store_model_id":4},{"rank":1,"subfolder":"rank1","store_model_id":9}]` {
		t.Fatalf("rank_weights wire = %s", got)
	}
	decoded, err := DecodeCatalogSelection(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := decoded.RankWeightModelID(1); !ok || id != 9 || !decoded.RankScoped() {
		t.Fatalf("decoded rank 1 = %d %v", id, ok)
	}
	if _, ok := decoded.RankWeightModelID(2); ok {
		t.Fatal("an unbound rank resolved")
	}
	old, err := DecodeCatalogSelection([]byte(`{"model_id":"m","engine":"vllm","weight_subfolder":"rank0"}`))
	if err != nil || old.RankScoped() || len(old.Extra) != 0 {
		t.Fatalf("old snapshot = %+v %v", old, err)
	}
}
