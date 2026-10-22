package dto

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestAcquisitionImageCacheLiteralWireRoundTrip(t *testing.T) {
	for _, literal := range []string{
		`{"image":"uncatalogued:v1","cached":true}`,
		`{"image":"uncatalogued:v1","cached":false}`,
		`{"image":"uncatalogued:v1","cache_reason":"query_failed"}`,
		`{"image":"uncatalogued:v1","cache_reason":"node_offline"}`,
	} {
		var decoded AcquisitionImage
		if err := json.Unmarshal([]byte(literal), &decoded); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(decoded)
		if err != nil {
			t.Fatal(err)
		}
		var want, got map[string]any
		if err := json.Unmarshal([]byte(literal), &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("wire=%s want=%s", encoded, literal)
		}
	}
}
