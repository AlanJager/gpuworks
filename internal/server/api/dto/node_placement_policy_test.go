package dto

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/nodepolicy"
)

// Every knob of the policy appears on the node, keyed as the dictionary keys
// it, and a value of a kind the DTO does not know is refused rather than
// written untyped.
func TestNodePlacementPolicyKeysMatchTheDictionary(t *testing.T) {
	values, availability := NodePlacementPolicyOf(nodepolicy.Policy{ShareWithForeign: true}, true, true)
	dictionary := NodePlacementPolicyDictionaryOf()
	if len(values) != len(dictionary) || len(availability) != len(dictionary) {
		t.Fatalf("values %v, availability %v, dictionary %v", values, availability, dictionary)
	}
	for _, setting := range dictionary {
		if _, ok := values[setting.Key]; !ok {
			t.Fatalf("knob %q has no value on the node", setting.Key)
		}
	}
	raw, err := json.Marshal(values)
	if err != nil || string(raw) != `{"share_with_foreign":true}` {
		t.Fatalf("values = %s %v", raw, err)
	}
	if raw, _ := json.Marshal(availability); string(raw) != `[{"key":"share_with_foreign","applicable":true}]` {
		t.Fatalf("availability = %s", raw)
	}
	if _, err := json.Marshal(NodePlacementPolicyValue{Kind: "colour"}); err == nil {
		t.Fatal("a value of unknown kind was written")
	}
}
