package proto

import (
	"encoding/json"
	"testing"
)

func TestRequestedFeaturesWireDistinguishesLegacyUnknownAndKnown(t *testing.T) {
	for _, tc := range []struct {
		wire           string
		present, known bool
	}{
		{`{"launch":{"argv":[],"model_mount":"/models"}}`, false, false},
		{`{"launch":{"argv":[],"model_mount":"/models","requested_features":{"command_known":false}}}`, true, false},
		{`{"launch":{"argv":[],"model_mount":"/models","requested_features":{"command_known":true,"pipeline_parallelism":true}}}`, true, true},
	} {
		var spec InstanceDesiredState
		if err := json.Unmarshal([]byte(tc.wire), &spec); err != nil {
			t.Fatal(err)
		}
		if (spec.RequestedFeatures != nil) != tc.present {
			t.Fatalf("presence for %s", tc.wire)
		}
		if tc.present && spec.RequestedFeatures.CommandKnown != tc.known {
			t.Fatalf("known for %s", tc.wire)
		}
		encoded, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		var round InstanceDesiredState
		if err := json.Unmarshal(encoded, &round); err != nil {
			t.Fatal(err)
		}
		if (round.RequestedFeatures != nil) != tc.present {
			t.Fatalf("lost optional snapshot: %s", encoded)
		}
	}
}
