//go:build linux

package extplugin

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/pluginbridge"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// artifactCheck reports what is left of package id in one per-package
// artifact; empty when nothing is.
type artifactCheck func(t *testing.T, s *site, sv *server, id string) string

// pstateRows counts id's rows in a pstate table.
func pstateRows(table string) artifactCheck {
	return func(t *testing.T, _ *site, sv *server, id string) string {
		var n int64
		if err := sv.a.Store().DB().Table(table).Where("owner = ?", id).Count(&n).Error; err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return strings.Repeat("row ", int(n))
		}
		return ""
	}
}

// underPackage reports whether <data>/plugins/<id>/<name> exists.
func underPackage(name string) artifactCheck {
	return func(t *testing.T, s *site, _ *server, id string) string {
		if _, err := os.Lstat(filepath.Join(layout.New(s.dir).Package(id), name)); err == nil {
			return name + " exists"
		} else if !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		return ""
	}
}

// listed reports whether the administration still lists or answers for
// id: the list, its report and its logs.
func listed(t *testing.T, _ *site, sv *server, id string) string {
	if p := sv.plugin(id); p.State != "absent" {
		return "listed as " + p.State
	}
	for _, path := range []string{"/state", "/logs"} {
		if status, body := sv.do(http.MethodGet, "/api/v1/plugins/"+id+path, nil); status != http.StatusNotFound {
			return path + " answers " + string(body)
		}
	}
	return ""
}

// enforced reports whether what id pushed is still enforced or shown: a
// banner, a feature, the plugins check naming it.
func enforced(t *testing.T, _ *site, sv *server, id string) string {
	m := sv.manifest()
	for _, b := range m.Banners {
		if strings.Contains(b.ID, id) {
			return "banner " + b.ID
		}
	}
	for _, f := range m.Features {
		if strings.HasPrefix(f.Key, "acme.") {
			return "feature " + f.Key
		}
	}
	return ""
}

// served reports whether the route table still has a route of id.
func served(t *testing.T, _ *site, sv *server, id string) string {
	if lines := pluginLines(t, sv, id); len(lines) > 0 {
		return strings.Join(lines, "; ")
	}
	return ""
}

// counted reports whether the plugin metrics still have a series of id
// (the plugin host's instance and counters).
func counted(t *testing.T, _ *site, sv *server, id string) string {
	status, body := sv.do(http.MethodGet, "/api/v1/metrics/plugins", nil)
	if status != http.StatusOK {
		t.Fatalf("metrics = %d %s", status, body)
	}
	if strings.Contains(string(body), `id="`+id+`"`) {
		return "metric series"
	}
	return ""
}

// marked reports whether the data marker plugin.<id>.data is stored.
func marked(t *testing.T, _ *site, sv *server, id string) string {
	markers, err := sv.a.Store().PluginMarkers()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range markers {
		if m.Name == extapi.PluginDataMarker(id) {
			return m.Name
		}
	}
	return ""
}

// perPackageChecks is how the contract observes each per-package artifact
// (pluginbridge.PerPackageArtifacts). Rows and files are read directly;
// structures in memory through what the server serves from them (the
// administration routes, the console manifest, the route inventory and
// the metrics). Several memory entries (host.logs, state.spawn,
// state.ready, bridge.pushers, bridge.refusals) can only be seen here
// through the package no longer being listed, which cannot fail on the
// structure itself; pluginbridge's TestUninstallLeavesNothingInMemory
// reads every structure directly and is their real check.
var perPackageChecks = map[string]artifactCheck{
	"db:plugin_markers":     marked,
	"file:pkg":              underPackage("pkg"),
	"file:control.json":     underPackage("control.json"),
	"file:upgrade.json":     underPackage("upgrade.json"),
	"file:data":             underPackage("data"),
	"file:tmp":              underPackage("tmp"),
	"file:snapshots":        underPackage("snapshots"),
	"memory:bridge.entries": listed, "memory:bridge.pushers": listed, "memory:bridge.refusals": listed,
	"memory:bridge.crashloops": listed, "memory:bridge.pending": listed,
	"memory:bridge.known": served, "memory:bridge.routes": served,
	"memory:bridge.banners": enforced, "memory:features": enforced, "memory:state.reg": enforced,
	"memory:host.plugins": counted, "memory:host.metrics": counted, "memory:host.logs": listed,
	"memory:state.parts": enforced, "memory:state.spawn": listed, "memory:state.ready": listed,
	"memory:state.down": enforced, "memory:state.downDirty": enforced,
	"memory:state.overrides": enforced, "memory:state.superseded": enforced,
}

func checkOf(a pluginbridge.Artifact) artifactCheck {
	if c, ok := perPackageChecks[a.Name]; ok {
		return c
	}
	if table, ok := strings.CutPrefix(a.Name, "db:pstate_"); ok {
		return pstateRows("pstate_" + table)
	}
	return nil
}

// leftovers names what is left of id in each artifact; afterPurge counts
// the artifacts only a purge removes.
func leftovers(t *testing.T, s *site, sv *server, id string, afterPurge bool) []string {
	t.Helper()
	var out []string
	for _, a := range pluginbridge.PerPackageArtifacts {
		if a.Purge && !afterPurge {
			continue
		}
		if left := checkOf(a)(t, s, sv, id); left != "" {
			out = append(out, a.Name+": "+left)
		}
	}
	return out
}

// D3: a package that uses every per-package feature (every table kind,
// routes, a feature key, storage with its data marker, an override of its
// hold) is uninstalled, then purged; each artifact in
// pluginbridge.PerPackageArtifacts is checked to be gone: the rows and
// overrides at the uninstall, the data directory and data marker at the
// purge.
func TestUninstallLeavesNothingUnderThePackageID(t *testing.T) {
	for _, a := range pluginbridge.PerPackageArtifacts {
		if checkOf(a) == nil {
			t.Fatalf("per-package artifact %s has no check here: add one to perPackageChecks", a.Name)
		}
	}
	s := newSite(t)
	sv := s.boot()
	st := sv.a.Store()
	h := node(t, st, "h", 2)
	s.args("acme_fake", "--requires", requiresAll(),
		"--push", "availability="+availabilityBody,
		"--push", "fleet_capacity="+`{"select":"enrollment_order","refuse_node":{"code":"ACME_NODE_LIMIT"},"refuse_card":{"code":"ACME_CARD_LIMIT"},"hold":{"code":"ACME_OVER_CAPACITY"},"keep":[`+jsonID(h.ID)+`],"steps":[{"max_cards":4}]}`,
		"--push", "node_holds="+holdsBody(h.ID),
		"--push", "banners="+bannersBody,
		"--push", "attestations="+attestationsBody)
	sv.install(s.fakePackage("acme_fake", "1.0.0", withFakeRoutes, func(spec *extsign.Spec) {
		spec.Manifest.Storage = &extapi.StorageDecl{Kind: extapi.StorageSQLite, HA: extapi.StorageHAShipped, StateVersion: 1}
	}), true)
	if got := s.waitResults("acme_fake", 5); !got[4].OK {
		t.Fatalf("pushes = %+v", got)
	}
	sv.waitPlugin("acme_fake", "running", func(p dto.Plugin) bool { return p.State == "running" })
	waitFor(t, "node h held", func() bool { return heldByPlugins(t, st, h.UUID) })
	waitFor(t, "the data marker recorded", func() bool { return marked(t, s, sv, "acme_fake") != "" })
	if err := os.MkdirAll(layout.New(s.dir).Snapshots("acme_fake"), 0o700); err != nil {
		t.Fatal(err)
	}
	if status, body := sv.do(http.MethodPost, "/api/v1/plugins/acme_fake/disable", nil); status != http.StatusOK {
		t.Fatalf("disable = %d %s", status, body)
	}
	sv.overrideHold("acme_fake", h.ID, "maintenance finished")
	if left := leftovers(t, s, sv, "acme_fake", true); len(left) < len(pluginbridge.PerPackageArtifacts)/2 {
		t.Fatalf("before the uninstall only %d artifacts hold something of the package; the check would prove nothing: %v", len(left), left)
	}

	im := sv.uninstall("acme_fake", false)
	if len(im.Tables) != 5 || len(im.Overrides) != 1 || len(im.Features) != 1 || len(im.Caps) != 1 {
		t.Fatalf("confirmed impact = %+v", im)
	}
	if left := leftovers(t, s, sv, "acme_fake", false); len(left) != 0 {
		t.Fatalf("after the uninstall: %v", left)
	}
	// The data and its marker stay for a reinstall until the purge (Q14).
	for _, kept := range []string{"file:data", "db:plugin_markers"} {
		for _, a := range pluginbridge.PerPackageArtifacts {
			if a.Name == kept && checkOf(a)(t, s, sv, "acme_fake") == "" {
				t.Fatalf("an uninstall without purge removed %s", kept)
			}
		}
	}
	if snap, err := st.LoadPushedState(context.Background()); err != nil || len(snap.Owners)+len(snap.Down)+len(snap.Overrides) != 0 {
		t.Fatalf("pushed state after the uninstall = %+v, %v", snap.Owners, err)
	}

	// The purge of the uninstalled package removes what only a purge
	// removes: its directory, data included, and its data marker.
	if im := sv.uninstall("acme_fake", true); len(im.Tables)+len(im.Nodes)+len(im.Overrides) != 0 {
		t.Fatalf("impact of the purge = %+v", im)
	}
	if left := leftovers(t, s, sv, "acme_fake", true); len(left) != 0 {
		t.Fatalf("after the purge: %v", left)
	}
	if status, body := sv.do(http.MethodGet, "/api/v1/plugins/acme_fake/impact", nil); status != http.StatusNotFound {
		t.Fatalf("impact after the purge = %d %s", status, body)
	}
}

// D3, purge: an uninstall with purge leaves nothing at all under the id,
// the data marker included.
func TestUninstallWithPurgeLeavesNothingUnderThePackageID(t *testing.T) {
	s := newSite(t)
	sv := s.boot()
	st := sv.a.Store()
	h := node(t, st, "h", 2)
	s.args("acme_fake", "--requires", requiresAll(), "--push", "node_holds="+holdsBody(h.ID), "--push", "banners="+bannersBody)
	sv.install(s.fakePackage("acme_fake", "1.0.0", func(spec *extsign.Spec) {
		spec.Manifest.Storage = &extapi.StorageDecl{Kind: extapi.StorageSQLite, HA: extapi.StorageHAShipped, StateVersion: 1}
	}), true)
	s.waitResults("acme_fake", 2)
	waitFor(t, "the data marker recorded", func() bool { return marked(t, s, sv, "acme_fake") != "" })
	sv.uninstall("acme_fake", true)
	if left := leftovers(t, s, sv, "acme_fake", true); len(left) != 0 {
		t.Fatalf("after the purge: %v", left)
	}
	if _, err := os.Lstat(layout.New(s.dir).Package("acme_fake")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the package directory after the purge: %v", err)
	}
}

func jsonID(id uint) string {
	b, _ := json.Marshal(id)
	return string(b)
}
