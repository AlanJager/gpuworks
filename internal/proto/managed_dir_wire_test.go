package proto

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestManagedDirGroupKeyRoundTrip(t *testing.T) {
	key := ManagedDirGroupKey(7, 1_700_000_000)
	if key != "7-1700000000" {
		t.Fatalf("key = %q", key)
	}
	id, created, ok := ParseManagedDirGroupKey(key)
	if !ok || id != 7 || created != 1_700_000_000 {
		t.Fatalf("parsed = %d %d %v", id, created, ok)
	}
	if ManagedDirGroupKey(0, 1) != "" {
		t.Fatal("deployment 0 has a group")
	}
	for _, bad := range []string{"", "7", "-1", "0-1", "7-", "07-1", "7-01", "7-1-2", "x-1", "7-x", "../7-1", "7-1/..", "4294967296-1"} {
		if _, _, ok := ParseManagedDirGroupKey(bad); ok {
			t.Fatalf("%q parsed as a group", bad)
		}
	}
}

func TestValidManagedDirName(t *testing.T) {
	for _, good := range []string{"a", "pico-state", "x1", "a-b-c", "abcdefghijklmnopqrstuvwxyz012345"} {
		if !ValidManagedDirName(good) {
			t.Fatalf("%q refused", good)
		}
	}
	for _, bad := range []string{"", "-a", "a-", "A", "a_b", "a.b", "a/b", "..", "abcdefghijklmnopqrstuvwxyz0123456"} {
		if ValidManagedDirName(bad) {
			t.Fatalf("%q accepted", bad)
		}
	}
}

// The wire keys both sides read, literally, and that an older peer's message
// without them decodes to none.
func TestManagedDirWireFields(t *testing.T) {
	cases := []struct {
		value any
		want  string
	}{
		{MountSpec{Target: "/pico", Purpose: MountPurposeManagedDir, SizeHint: "1", ManagedDir: &ManagedDirRef{Group: "7-1", Name: "pico-state"}},
			`{"target":"/pico","size_hint":"1","purpose":"managed-dir","managed_dir":{"group":"7-1","name":"pico-state"}}`},
		{MountSpec{Target: "/data"}, `{"target":"/data"}`},
		{StateUpdate{ReleasedDirDeployments: []uint{7}}, `{"image_refs":null,"released_dir_deployments":[7]}`},
		{PathUsage{Kind: PathKindManagedDirs, Path: "/s", TotalBytes: 2, AvailableBytes: 1, SharesDiskWith: []string{"models"}},
			`{"kind":"managed_dirs","path":"/s","total_bytes":2,"available_bytes":1,"shares_disk_with":["models"]}`},
	}
	for _, test := range cases {
		raw, err := json.Marshal(test.value)
		if err != nil || string(raw) != test.want {
			t.Fatalf("wire = %s (%v), want %s", raw, err, test.want)
		}
	}
	var hello Hello
	if err := json.Unmarshal([]byte(`{"proto_min":1,"proto_max":1,"version":"v","managed_dir_groups":["7-1"]}`), &hello); err != nil || !reflect.DeepEqual(hello.ManagedDirGroups, []string{"7-1"}) {
		t.Fatalf("hello = %+v %v", hello, err)
	}
	var ds DesiredState
	if err := json.Unmarshal([]byte(`{"instances":[],"model_files":[],"network":{},"storages":[],"released_dir_groups":["7-1"]}`), &ds); err != nil || !reflect.DeepEqual(ds.ReleasedDirGroups, []string{"7-1"}) {
		t.Fatalf("desired state = %+v %v", ds.ReleasedDirGroups, err)
	}
	ds = DesiredState{}
	if err := json.Unmarshal([]byte(`{"instances":[],"model_files":[],"network":{},"storages":[]}`), &ds); err != nil || ds.ReleasedDirGroups != nil {
		t.Fatalf("older server's state = %+v %v", ds.ReleasedDirGroups, err)
	}
}
