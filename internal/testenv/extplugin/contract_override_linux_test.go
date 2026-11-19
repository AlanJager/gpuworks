//go:build linux

package extplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// twoHolds holds nodes a and b with codes ca and cb.
func twoHolds(a, b uint, ca, cb string) string {
	return fmt.Sprintf(`{"nodes":{"%d":{"code":"%s"},"%d":{"code":"%s"}}}`, a, ca, b, cb)
}

func (sv *server) overrides() []dto.PluginOverride {
	sv.t.Helper()
	status, body := sv.do(http.MethodGet, "/api/v1/plugins/overrides", nil)
	var list dto.PluginOverrideList
	if status != http.StatusOK || json.Unmarshal(body, &list) != nil {
		sv.t.Fatalf("overrides = %d %s", status, body)
	}
	return list.Items
}

func (sv *server) overrideHold(owner string, nodeID uint, reason string) dto.PluginOverride {
	sv.t.Helper()
	req := fmt.Sprintf(`{"owner":%q,"kind":"node_holds","entry":"%d","action":"delete","reason":%q}`, owner, nodeID, reason)
	status, body := sv.do(http.MethodPost, "/api/v1/plugins/overrides", strings.NewReader(req))
	var o dto.PluginOverride
	if status != http.StatusCreated || json.Unmarshal(body, &o) != nil || o.ID == 0 {
		sv.t.Fatalf("override of node %d = %d %s", nodeID, status, body)
	}
	return o
}

// restartWith stops plugin id, sets how it behaves and starts it again.
func (sv *server) restartWith(s *site, id string, lines ...string) {
	sv.t.Helper()
	if status, body := sv.do(http.MethodPost, "/api/v1/plugins/"+id+"/disable", nil); status != http.StatusOK {
		sv.t.Fatalf("disable %s = %d %s", id, status, body)
	}
	s.args(id, lines...)
	if status, body := sv.do(http.MethodPost, "/api/v1/plugins/"+id+"/enable", nil); status != http.StatusOK {
		sv.t.Fatalf("enable %s = %d %s", id, status, body)
	}
}

func bannerIDs(m dto.ConsoleManifest) []string {
	var out []string
	for _, b := range m.Banners {
		out = append(out, b.ID)
	}
	return out
}

// D1: an administrator releases a hold of a dead plugin through the API;
// the plugin's push that keeps the entry keeps the override, its push that
// changes the entry supersedes it with a banner; a package that no longer
// verifies can still be overridden; uninstall drops the override.
func TestContractOverrideOfAPluginsHold(t *testing.T) {
	s := newSite(t)
	sv := s.boot()
	st := sv.a.Store()
	a, b := node(t, st, "a", 2), node(t, st, "b", 2)
	s.args("acme_fake", "--requires", requiresAll(), "--push", "node_holds="+twoHolds(a.ID, b.ID, "ACME_MAINTENANCE", "ACME_MAINTENANCE"))
	sv.install(s.fakePackage("acme_fake", "1.0.0"), true)
	if r := s.waitResults("acme_fake", 1)[0]; !r.OK {
		t.Fatalf("push = %+v", r)
	}
	waitFor(t, "both nodes held", func() bool { return heldByPlugins(t, st, a.UUID) && heldByPlugins(t, st, b.UUID) })

	// The plugin is dead: an override releases node a only.
	if status, body := sv.do(http.MethodPost, "/api/v1/plugins/acme_fake/disable", nil); status != http.StatusOK {
		t.Fatalf("disable = %d %s", status, body)
	}
	o := sv.overrideHold("acme_fake", a.ID, "maintenance finished")
	if o.Owner != "acme_fake" || o.Kind != "node_holds" || o.Action != "delete" || o.ByUser == 0 || o.Reason != "maintenance finished" {
		t.Fatalf("override = %+v", o)
	}
	waitFor(t, "node a schedulable", func() bool { return !heldByPlugins(t, st, a.UUID) })
	if !heldByPlugins(t, st, b.UUID) {
		t.Fatal("the override released node b too")
	}

	// It starts again and pushes node a's entry as it was: the override
	// stays.
	s.args("acme_fake", "--requires", requiresAll(), "--push", "node_holds="+twoHolds(a.ID, b.ID, "ACME_MAINTENANCE", "ACME_OVER_CAPACITY"))
	if status, body := sv.do(http.MethodPost, "/api/v1/plugins/acme_fake/enable", nil); status != http.StatusOK {
		t.Fatalf("enable = %d %s", status, body)
	}
	if r := s.waitResults("acme_fake", 2)[1]; !r.OK {
		t.Fatalf("second push = %+v", r)
	}
	if list := sv.overrides(); len(list) != 1 || list[0].ID != o.ID {
		t.Fatalf("overrides after a push that kept the entry = %+v", list)
	}
	if heldByPlugins(t, st, a.UUID) {
		t.Fatal("a push that kept node a's entry superseded the override")
	}

	// It pushes a changed entry: its decision wins and administrators are
	// told.
	sv.restartWith(s, "acme_fake", "--requires", requiresAll(), "--push", "node_holds="+twoHolds(a.ID, b.ID, "ACME_OVER_CAPACITY", "ACME_OVER_CAPACITY"))
	if r := s.waitResults("acme_fake", 3)[2]; !r.OK {
		t.Fatalf("third push = %+v", r)
	}
	waitFor(t, "node a held again", func() bool { return heldByPlugins(t, st, a.UUID) })
	if list := sv.overrides(); len(list) != 0 {
		t.Fatalf("overrides after a push that changed the entry = %+v", list)
	}
	superseded := fmt.Sprintf("plugins.override_superseded.%d", o.ID)
	var banner *dto.ConsoleBanner
	m := sv.manifest()
	for i := range m.Banners {
		if m.Banners[i].ID == superseded {
			banner = &m.Banners[i]
		}
	}
	if banner == nil || banner.Message.Code != messages.ErrorPluginOverrideSuperseded {
		t.Fatalf("banners = %v; want %s", bannerIDs(m), superseded)
	}

	// The package no longer verifies: its holds stay, and an override
	// still releases one.
	sv.stop()
	bin := filepath.Join(layout.New(s.dir).Version("acme_fake", "1.0.0"), "bin", "linux_"+runtime.GOARCH, "acme_fake")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	sv = s.boot()
	st = sv.a.Store()
	if p := sv.plugin("acme_fake"); p.State != "broken" {
		t.Fatalf("tampered package = %+v", p)
	}
	if !heldByPlugins(t, st, a.UUID) || !heldByPlugins(t, st, b.UUID) {
		t.Fatal("a broken package's holds were released")
	}
	broken := sv.overrideHold("acme_fake", b.ID, "the package is broken")
	waitFor(t, "node b schedulable", func() bool { return !heldByPlugins(t, st, b.UUID) })

	// Uninstall drops the override with everything the package pushed.
	sv.uninstall("acme_fake", false)
	if list := sv.overrides(); len(list) != 0 {
		t.Fatalf("overrides after uninstall = %+v", list)
	}
	if snap, err := st.LoadPushedState(context.Background()); err != nil || len(snap.Overrides) != 0 {
		t.Fatalf("stored overrides after uninstall = %+v, %v", snap.Overrides, err)
	}
	status, body := sv.do(http.MethodDelete, fmt.Sprintf("/api/v1/plugins/overrides/%d", broken.ID), nil)
	if status != http.StatusNotFound || !strings.Contains(string(body), messages.ErrorPluginOverrideNotFound) {
		t.Fatalf("removal of an uninstalled package's override = %d %s", status, body)
	}
}
