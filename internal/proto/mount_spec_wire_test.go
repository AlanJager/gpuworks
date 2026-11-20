package proto

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestMountSpecVolumeWireJSON pins the wire keys of a named-volume mount: the
// server states the volume under "volume" and no "source", and the agent must
// read it back unchanged, or the runtime is handed an empty source.
func TestMountSpecVolumeWireJSON(t *testing.T) {
	const wire = `{"volume":"myvol","target":"/data","read_only":true,"purpose":"cache"}`
	var decoded MountSpec
	if err := json.Unmarshal([]byte(wire), &decoded); err != nil {
		t.Fatal(err)
	}
	want := MountSpec{Volume: "myvol", Target: "/data", ReadOnly: true, Purpose: "cache"}
	if decoded != want {
		t.Fatalf("decoded %+v, want %+v", decoded, want)
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != wire {
		t.Fatalf("encoded %s, want %s", encoded, wire)
	}

	var desired InstanceDesiredState
	if err := json.Unmarshal([]byte(`{"launch":{"argv":[],"model_mount":"","mounts":[{"source":"/srv/a","target":"/a"},`+wire+`]}}`), &desired); err != nil {
		t.Fatal(err)
	}
	if got := desired.Mounts; !reflect.DeepEqual(got, []MountSpec{{Source: "/srv/a", Target: "/a"}, want}) {
		t.Fatalf("desired mounts = %+v", got)
	}
}

func TestValidVolumeName(t *testing.T) {
	for _, name := range []string{"myvol", "hf-cache", "a1", "data_v2.0"} {
		if !ValidVolumeName(name) {
			t.Errorf("%q refused", name)
		}
	}
	for _, name := range []string{"", "a", "/srv/data", "vol:ro", "vol/sub", "-vol", ".vol", "my vol", "../x"} {
		if ValidVolumeName(name) {
			t.Errorf("%q accepted", name)
		}
	}
}
