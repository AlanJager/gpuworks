package compat

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

func TestCheckStartupRefusesUnknownAndNewerCoreMarkers(t *testing.T) {
	if err := CheckStartup(nil); err != nil {
		t.Fatalf("no markers = %v", err)
	}
	// Package data markers never stop the core.
	if err := CheckStartup([]extapi.Marker{{Name: extapi.PluginDataMarker("acme_x"), Version: 9, WrittenBy: "v29.0.0"}}); err != nil {
		t.Fatalf("package marker stopped the core: %v", err)
	}
	// A core marker this build does not know is newer than it.
	err := CheckStartup([]extapi.Marker{
		{Name: "pstate.node_holds", Version: 1, WrittenBy: "v27.2.0"},
		{Name: "pstate.availability", Version: 2, WrittenBy: "v27.4.1"},
	})
	if extapi.CodeOf(err) != extapi.ErrorStateTooNew {
		t.Fatalf("unknown core marker = %v", err)
	}
	var xe *extapi.Error
	if !errors.As(err, &xe) || xe.Params["needs"] != "v27.4.1" || xe.Params["marker"] != "pstate.availability" || xe.Params["count"] != 2 {
		t.Fatalf("params = %v", xe.Params)
	}
}

func TestCheckReportsEveryFinding(t *testing.T) {
	stored := []extapi.Marker{
		{Name: "pstate.availability", Version: 2, WrittenBy: "v27.4.0"},
		{Name: extapi.PluginDataMarker("acme_a"), Version: 3, WrittenBy: "v27.4.0"},
		{Name: extapi.PluginDataMarker("acme_b"), Version: 1, WrittenBy: "v27.0.0"},
		{Name: extapi.PluginDataMarker("acme_gone"), Version: 7, WrittenBy: "v27.0.0"},
	}
	pkgs := []Package{
		{ID: "acme_a", Version: "1.0.0", Manifest: &extapi.Manifest{Storage: &extapi.StorageDecl{StateVersion: 2}}},
		{ID: "acme_b", Version: "1.0.0", Manifest: &extapi.Manifest{Storage: &extapi.StorageDecl{StateVersion: 1}, Requires: map[string]int{"table.availability": 1, "node.app": 1}, MinCoreHint: "v27.3"}},
		{ID: "acme_c", Version: "2.0.0", ManifestErr: &extapi.Error{Code: extapi.ErrorManifestTooNew, Params: map[string]any{"min_core_hint": "v29.1"}, Message: "too new"}},
	}
	r := Check(stored, map[string]int{"pstate.availability": 1}, map[string]int{"table.availability": 1}, pkgs)
	if r.OK() || len(r.Blocking) != 1 || r.Blocking[0].Subject != "pstate.availability" || r.Blocking[0].Code != extapi.ErrorStateTooNew {
		t.Fatalf("blocking = %+v", r.Blocking)
	}
	want := []struct{ subject, code string }{
		{"acme_a", extapi.ErrorStateTooNew}, {"acme_b", messages.ErrorPluginIncompatible}, {"acme_c", extapi.ErrorManifestTooNew},
	}
	if len(r.Packages) != len(want) {
		t.Fatalf("package findings = %+v", r.Packages)
	}
	for i, w := range want {
		if r.Packages[i].Subject != w.subject || r.Packages[i].Code != w.code {
			t.Fatalf("finding %d = %+v, want %v", i, r.Packages[i], w)
		}
	}
	if r.Packages[1].Params["capability"] != "node.app" {
		t.Fatalf("incompatible params = %v", r.Packages[1].Params)
	}
	if ok := Check(nil, nil, nil, nil); !ok.OK() {
		t.Fatal("an empty data directory is not compatible")
	}
}

func TestInstalledReadsCurrentManifests(t *testing.T) {
	d := layout.New(t.TempDir())
	write := func(id, version, manifest string) {
		dir := d.Version(id, version)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if manifest != "" {
			if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o444); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := d.Activate(id, version); err != nil {
			t.Fatal(err)
		}
	}
	write("acme_a", "1.0.0", `{"created":"2026-11-18T00:00:00Z","files":[{"path":"locales/en.json","sha256":"44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2}],"id":"acme_a","protocol":{"max":1,"min":1},"publisher":"acme","schema":1,"storage":{"ha":"shipped","kind":"sqlite","state_version":2},"version":"1.0.0"}`)
	write("acme_b", "1.0.0", `{"min_core_hint":"v29.1","schema":2}`)
	write("acme_c", "1.0.0", "")
	pkgs, err := Installed(d)
	if err != nil || len(pkgs) != 3 {
		t.Fatalf("installed = %+v %v", pkgs, err)
	}
	if pkgs[0].Manifest == nil || pkgs[0].Manifest.Storage.StateVersion != 2 {
		t.Fatalf("acme_a = %+v", pkgs[0])
	}
	if extapi.CodeOf(pkgs[1].ManifestErr) != extapi.ErrorManifestTooNew {
		t.Fatalf("acme_b = %+v", pkgs[1])
	}
	if !os.IsNotExist(pkgs[2].ManifestErr) {
		t.Fatalf("acme_c = %+v", pkgs[2])
	}
}
