package proto

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestStateUpdateNetworkDistinguishesAbsentAndClear(t *testing.T) {
	for _, input := range []string{`{}`, `{"network":{}}`, `{"network":{"http_proxy":"http://proxy.invalid","serving_ip":"192.0.2.1","engine_ports":{"start":8000,"end":8100}}}`} {
		var update StateUpdate
		if err := json.Unmarshal([]byte(input), &update); err != nil {
			t.Fatal(err)
		}
		output, err := json.Marshal(update)
		if err != nil {
			t.Fatal(err)
		}
		var before, after map[string]any
		_ = json.Unmarshal([]byte(input), &before)
		_ = json.Unmarshal(output, &after)
		if !reflect.DeepEqual(before["network"], after["network"]) {
			t.Fatalf("network wire value lost: input=%s output=%s", input, output)
		}
	}
}
