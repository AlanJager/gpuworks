package dto

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestServerOutboundPolicyWireContract(t *testing.T) {
	const wire = `{"restrict_private_hosts":false,"allow_private_hosts":[],"editable":true,"revision":"opaque"}`
	original := ServerOutboundPolicy{AllowPrivateHosts: []string{}, Editable: true, Revision: "opaque"}
	raw, err := json.Marshal(original)
	if err != nil || string(raw) != wire {
		t.Fatalf("wire %s err %v", raw, err)
	}
	var decoded ServerOutboundPolicy
	if err = json.Unmarshal([]byte(wire), &decoded); err != nil || !reflect.DeepEqual(original, decoded) {
		t.Fatalf("round trip %+v err %v", decoded, err)
	}
	var update ServerOutboundPolicyUpdate
	if err = json.Unmarshal([]byte(`{"restrict_private_hosts":false,"allow_private_hosts":[],"revision":"opaque"}`), &update); err != nil || update.RestrictPrivateHosts == nil || *update.RestrictPrivateHosts || update.AllowPrivateHosts == nil || update.Revision != "opaque" {
		t.Fatalf("update %+v %v", update, err)
	}
	if err = json.Unmarshal([]byte(`{}`), &update); err != nil {
		t.Fatal(err)
	}
}
