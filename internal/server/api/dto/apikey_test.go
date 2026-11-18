package dto

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAPIKeyOwnershipWireKeepsAnExplicitZero(t *testing.T) {
	const wire = `{"user_id":0,"name":"legacy","prefix":"gw-legacy000","created_at":"2026-11-18T00:00:00Z","all_deployments":true,"deployments":[],"priority":{"class":"","ceiling":"","policy":"within_ceiling"}}`
	for _, value := range []string{wire, strings.Replace(wire, `"user_id":0`, `"user_id":7`, 1)} {
		var key APIKey
		if err := json.Unmarshal([]byte(value), &key); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(key)
		if err != nil || string(encoded) != value {
			t.Fatalf("ownership wire=%s, err=%v, want %s", encoded, err, value)
		}
	}
}
