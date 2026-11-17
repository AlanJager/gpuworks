package extapi

import (
	"errors"
	"testing"
)

func TestCapabilityRefs(t *testing.T) {
	ref, err := ParseCapabilityRef("table.availability@1")
	if err != nil || ref != (CapabilityRef{Name: "table.availability", Version: 1}) || ref.String() != "table.availability@1" {
		t.Fatalf("%+v %v", ref, err)
	}
	for _, bad := range []string{"table.availability", "table.availability@0", "table.availability@01", "Table@1", "routes@x", "@1"} {
		if _, err := ParseCapabilityRef(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if TableCapability(KindNodeHolds) != "table.node_holds" {
		t.Fatal(TableCapability(KindNodeHolds))
	}
	if got := TableKinds(); len(got) != 5 || got[0] != KindAttestations {
		t.Fatalf("%v", got)
	}
}

func TestCompareVersions(t *testing.T) {
	ordered := []string{"0.9.0", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2",
		"1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "v1.0.1", "1.2.0", "1.10.0", "26.12.0", "100.0.0"}
	for i := range ordered {
		for j := range ordered {
			c, err := CompareVersions(ordered[i], ordered[j])
			if err != nil {
				t.Fatal(err)
			}
			if want := compareInt(i, j); c != want {
				t.Errorf("compare(%s, %s) = %d, want %d", ordered[i], ordered[j], c, want)
			}
		}
	}
	if c, _ := CompareVersions("1.0.0+build.1", "1.0.0+build.2"); c != 0 {
		t.Error("build metadata counted")
	}
	if _, err := CompareVersions("1.0", "1.0.0"); err == nil {
		t.Error("1.0 accepted")
	}
}

// A server that meets state written by a newer one refuses to start and
// names the release that wrote it; it never drops or rewrites the state.
func TestCheckMarkersRefusesNewerState(t *testing.T) {
	known := map[string]int{"pstate.availability": 1, "pstate.node_holds": 1, "plugin.acme_backup.data": 3}
	if err := CheckMarkers([]Marker{
		{Name: "pstate.availability", Version: 1, WrittenBy: "v26.12.0"},
		{Name: "plugin.acme_backup.data", Version: 2, WrittenBy: "v26.11.0"},
	}, known); err != nil {
		t.Fatalf("older or equal state refused: %v", err)
	}
	err := CheckMarkers([]Marker{
		{Name: "pstate.node_holds", Version: 2, WrittenBy: "v27.3.0"},
		{Name: "pstate.availability", Version: 1, WrittenBy: "v26.12.0"},
		{Name: "pstate.quota_limits", Version: 1, WrittenBy: "v27.10.1"},
	}, known)
	var e *Error
	if !errors.As(err, &e) || e.Code != ErrorStateTooNew {
		t.Fatalf("got %v", err)
	}
	if e.Params["marker"] != "pstate.node_holds" || e.Params["version"] != 2 || e.Params["known"] != 1 ||
		e.Params["needs"] != "v27.10.1" || e.Params["count"] != 2 {
		t.Fatalf("params %v", e.Params)
	}
	if got := TooNew([]Marker{{Name: "pstate.quota_limits", Version: 1}}, known); len(got) != 1 {
		t.Fatalf("a marker this build does not know is not newer: %v", got)
	}
}

// A package's data marker is checked against the version the caller lists
// for it (the installed package's storage.state_version); the data of a
// package the caller does not list never stops the core.
func TestPluginDataMarkersFollowTheirPackage(t *testing.T) {
	core := map[string]int{"pstate.availability": 1}
	unlisted := []Marker{{Name: PluginDataMarker("acme_backup"), Version: 7, WrittenBy: "v27.1.0"}}
	if err := CheckMarkers(unlisted, core); err != nil {
		t.Fatalf("data of an unlisted package stopped the core: %v", err)
	}
	listed := map[string]int{"pstate.availability": 1, PluginDataMarker("acme_backup"): 6}
	if got := TooNew(unlisted, listed); len(got) != 1 || got[0].Name != "plugin.acme_backup.data" {
		t.Fatalf("data newer than the installed package: %v", got)
	}
	listed[PluginDataMarker("acme_backup")] = 7
	if got := TooNew(unlisted, listed); len(got) != 0 {
		t.Fatalf("data at the installed package's version: %v", got)
	}
}

func TestProtocolRange(t *testing.T) {
	if !(Range{Min: 1, Max: 3}).Contains(ProtocolMajor) || (Range{Min: 2, Max: 3}).Contains(ProtocolMajor) {
		t.Fatal("contains")
	}
	if err := checkProtocol(Range{Min: 2, Max: 2}); CodeOf(err) != ErrorProtocolUnsupported {
		t.Fatal(err)
	}
}
