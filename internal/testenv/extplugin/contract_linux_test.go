//go:build linux

package extplugin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/store/pstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Contract suite of the installable extensions (ADR 0071; each case is an
// invariant of docs/dev/extensions.md, "可安装扩展"). Run it with
//
//	go test ./internal/testenv/extplugin/ -run Contract -v

// appMounts are the routes a running server adds to the API's own
// inventory for the dependencies it wires (the console, the gateway, the
// playground's proxy, the install script), every method of each.
var appMounts = []string{"/*", "/v1/*", "/api/v1/playground/v1/*", "/install.sh"}

// With nothing installed the server is the community server: the API's
// golden route inventory (plugin administration among it) plus only the
// server's own mounts, and the community console manifest.
func TestContractNothingInstalledIsTheCommunityServer(t *testing.T) {
	sv := newSite(t).boot()
	var got []string
	all, err := sv.a.RouteInventory()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range all {
		_, pattern, _ := strings.Cut(line, " ")
		if !slices.Contains(appMounts, pattern) {
			got = append(got, line)
		}
	}
	want, err := api.RouteInventory(api.Router(api.Deps{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(all)-len(got) != 3*10+1 || !slices.Equal(got, want) {
		t.Fatalf("routes with nothing installed differ from the community routes:\n%s\nvs\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	status, body := sv.do(http.MethodGet, "/api/v1/console/manifest", nil)
	if status != http.StatusOK || strings.TrimSpace(string(body)) != `{"edition":"community","features":[],"nav":[],"banners":[]}` {
		t.Fatalf("manifest = %d %s", status, body)
	}
	if status, body := sv.do(http.MethodGet, "/api/v1/plugins", nil); status != http.StatusOK || strings.TrimSpace(string(body)) != `{"items":[]}` {
		t.Fatalf("plugins = %d %s", status, body)
	}
}

// A plugin that refuses initialize is left failed with its refusal, and is
// not restarted.
func TestContractHandshakeRefusal(t *testing.T) {
	s := newSite(t)
	sv := s.boot()
	s.args("acme_fake", "--refuse", "ACME_NOT_READY:failed_precondition")
	sv.install(s.fakePackage("acme_fake", "1.0.0"), true)
	p := sv.waitPlugin("acme_fake", "failed", func(p dto.Plugin) bool { return p.State == "failed" })
	if p.Message == nil || p.Message.Code != messages.ErrorPluginHandshakeRefused || p.Message.Params["plugin_code"] != "ACME_NOT_READY" {
		t.Fatalf("refused plugin = %+v", p)
	}
	// A start that timed out on a loaded host is retried; a refusal is
	// final.
	time.Sleep(500 * time.Millisecond)
	if again := sv.plugin("acme_fake"); again.State != "failed" || again.Restarts != p.Restarts {
		t.Fatalf("a refused plugin was restarted: %+v, then %+v", p, again)
	}
}

const (
	availabilityBody = `{"down_grace_s":60,"rows":[{"feature":"acme.backup","down_access":"read_only","steps":[{"access":"full"}]}]}`
	capacityBody     = `{"select":"enrollment_order","refuse_node":{"code":"ACME_NODE_LIMIT"},"refuse_card":{"code":"ACME_CARD_LIMIT","params":{"max":4}},"hold":{"code":"ACME_OVER_CAPACITY"},"steps":[{"max_cards":4}]}`
	bannersBody      = `{"banners":[{"id":"grace","audience":"admin","steps":[{"severity":"warning","notice":{"code":"ACME_GRACE","params":{"days":30}}}]}]}`
	attestationsBody = `{"attestations":[{"name":"grant","format":"acme.grant.v1","blob":"c3RhdGVtZW50"}]}`
)

func holdsBody(id uint) string {
	return fmt.Sprintf(`{"nodes":{"%d":{"code":"ACME_MAINTENANCE","params":{"ticket":"OPS-1"}}}}`, id)
}

// The whole life of an authority plugin: every kind pushed and enforced
// through the real hooks; stale, oversize, not-owner and non-authority
// pushes refused; the core killed and booted with the plugin dead,
// enforcing before the plugin starts and placing a node that joins; the
// grace not renewed by the restart and the clamp after it; uninstall
// releasing every hold.
func TestContractAuthorityPluginLifecycle(t *testing.T) {
	s := newSite(t)
	sv := s.boot()
	st := sv.a.Store()
	a := node(t, st, "a", 2)
	h := node(t, st, "h", 0)

	// Every kind pushed, then refusals: a stale generation, an envelope
	// and a table over their limits.
	var banners17 []string
	for i := range 17 {
		banners17 = append(banners17, fmt.Sprintf(`{"id":"b%d","audience":"all","steps":[{"severity":"info","notice":{"code":"ACME_GRACE"}}]}`, i))
	}
	s.args("acme_fake",
		"--requires", requiresAll(),
		"--push", "availability="+availabilityBody,
		"--push", "fleet_capacity="+capacityBody,
		"--push", "node_holds="+holdsBody(h.ID),
		"--push", "banners="+bannersBody,
		"--push", "attestations="+attestationsBody,
		"--push-raw", "banners@1="+bannersBody,
		"--push-raw", `banners@9={"banners":[`+strings.Join(banners17, ",")+`]}`,
		"--push-oversize", "banners")
	in := sv.install(s.fakePackage("acme_fake", "1.0.0"), true)
	if in.Plugin.ID != "acme_fake" || !in.Plugin.Enabled {
		t.Fatalf("installed = %+v", in)
	}
	got := s.waitResults("acme_fake", 8)
	for i, kind := range []string{"availability", "fleet_capacity", "node_holds", "banners", "attestations"} {
		if r := got[i]; r.Kind != kind || !r.OK {
			t.Fatalf("push %d = %+v, want %s applied", i, r, kind)
		}
	}
	for i, want := range []string{messages.ErrorPluginStateStale, extapi.ErrorStateTooLarge, extapi.ErrorStateTooLarge} {
		if r := got[5+i]; r.OK || r.Code != want {
			t.Fatalf("refused push %d = %+v, want %s", i, r, want)
		}
	}
	p := sv.waitPlugin("acme_fake", "running", func(p dto.Plugin) bool { return p.State == "running" })
	if p.Isolation != extpkg.HostIsolation() || p.Trust != "project" {
		t.Fatalf("running plugin = %+v", p)
	}

	// Enforced through the real hooks.
	waitFor(t, "node h held by its explicit hold", func() bool { return heldByPlugins(t, st, h.UUID) })
	if heldByPlugins(t, st, a.UUID) {
		t.Fatal("node a, within the capacity, is held")
	}
	m := sv.manifest()
	if len(m.Banners) != 1 || m.Banners[0].ID != "acme_fake.grace" || m.Banners[0].Message.Code != "ACME_GRACE" {
		t.Fatalf("banners = %+v", m.Banners)
	}
	if f := feature(m, "acme.backup"); f.Access != "full" {
		t.Fatalf("acme.backup of a package installed while the server runs = %+v", f)
	}
	snap, err := st.LoadPushedState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ownsKind(snap.Owners, "acme_fake", extapi.KindAttestations) {
		t.Fatalf("attestations were not persisted: %+v", snap.Owners)
	}

	// A second package of the same publisher answers for nothing: its
	// availability is not its own, and only the authority attests.
	s.args("acme_other", "--requires", requiresAll(),
		"--push", "availability="+availabilityBody, "--push", "attestations="+attestationsBody)
	sv.install(s.fakePackage("acme_other", "1.0.0"), true)
	other := s.waitResults("acme_other", 2)
	if other[0].OK || other[0].Code != messages.ErrorPluginStateNotOwner || other[1].OK || other[1].Code != messages.ErrorPluginStateNotPermitted {
		t.Fatalf("pushes of a non-authority package = %+v", other)
	}
	if status, _ := sv.do(http.MethodDelete, "/api/v1/plugins/acme_other?purge=true", nil); status != http.StatusNoContent {
		t.Fatalf("uninstall of acme_other = %d", status)
	}

	// Kill the core; boot it again with the plugin unable to start.
	sv.stop()
	s.clock.advance(100 * time.Second)
	s.args("acme_fake", "--exit-at-start", "3")
	booted := s.open()
	st = booted.Store()
	if !heldByPlugins(t, st, h.UUID) {
		t.Fatal("before any plugin started, the boot released the plugin's hold")
	}
	sv = s.run(booted)
	sv.waitPlugin("acme_fake", "down", func(p dto.Plugin) bool { return p.State == "backoff" || p.State == "crashloop" })
	if f := feature(sv.manifest(), "acme.backup"); f.Access != "full" {
		t.Fatalf("acme.backup with its authority dead, within the grace = %+v", f)
	}
	// What it pushed is shown, and the core tells administrators it is
	// down.
	if m := sv.manifest(); len(m.Banners) != 2 || m.Banners[0].ID != "acme_fake.grace" || m.Banners[1].Message.Code != messages.ErrorPluginDown {
		t.Fatalf("banners with the plugin dead = %+v", m.Banners)
	}

	// A node joins while the plugin is dead: the core places it.
	c := node(t, st, "c", 4)
	waitFor(t, "node c, past the cards cap, held by the core's selection", func() bool { return heldByPlugins(t, st, c.UUID) })
	if heldByPlugins(t, st, a.UUID) {
		t.Fatal("node a, enrolled first, lost its place to node c")
	}
	if _, _, err := st.CreateNodePending("d"); !errors.Is(err, store.ErrNodeEnrollmentRefused) {
		t.Fatalf("an enrollment over the cards cap = %v", err)
	}

	// The grace counts from when the plugin went down before the restart:
	// 60 s plus the two-minute boot extension. A disable makes pushed
	// state recombine at once instead of at its next timer.
	s.clock.advance(85 * time.Second)
	if status, body := sv.do(http.MethodPost, "/api/v1/plugins/acme_fake/disable", nil); status != http.StatusOK {
		t.Fatalf("disable = %d %s", status, body)
	}
	f := feature(sv.manifest(), "acme.backup")
	if f.Access != "read_only" || f.Message == nil || f.Message.Code != messages.ErrorPluginUnavailable {
		t.Fatalf("acme.backup past the grace = %+v", f)
	}

	// Uninstall releases what it held; running work is untouched.
	if status, body := sv.do(http.MethodDelete, "/api/v1/plugins/acme_fake", nil); status != http.StatusNoContent {
		t.Fatalf("uninstall = %d %s", status, body)
	}
	waitFor(t, "every hold released", func() bool { return !heldByPlugins(t, st, h.UUID) && !heldByPlugins(t, st, c.UUID) })
	if _, _, err := st.CreateNodePending("d"); err != nil {
		t.Fatalf("enrollment after uninstall = %v", err)
	}
	if m := sv.manifest(); len(m.Banners) != 0 || len(m.Features) != 0 {
		t.Fatalf("banners and features after uninstall = %+v, %+v", m.Banners, m.Features)
	}
	if snap, err := st.LoadPushedState(context.Background()); err != nil || len(snap.Owners) != 0 {
		t.Fatalf("pushed state after uninstall = %+v, %v", snap.Owners, err)
	}
}

// Under Landlock the plugin can write its own data directory but cannot
// open the server's master key.
func TestContractLandlockKeepsThePluginFromTheMasterKey(t *testing.T) {
	requireLandlock(t)
	s := newSite(t)
	sv := s.boot()
	key := filepath.Join(s.dir, "master.key")
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("the server has no master key at %s: %v", key, err)
	}
	s.args("acme_fake", "--probe-open", key)
	sv.install(s.fakePackage("acme_fake", "1.0.0"), true)
	r := s.waitResults("acme_fake", 1)[0]
	if r.Probe != "files" || r.Read != "EACCES" || r.Write != "" {
		t.Fatalf("probe = %+v, want the key refused and the data directory writable", r)
	}
}

func requiresAll() string {
	var out []string
	for _, kind := range extapi.TableKinds() {
		out = append(out, extapi.TableCapability(kind)+"@1-1")
	}
	return strings.Join(out, ",")
}

func ownsKind(owners []pstate.Owner, owner, kind string) bool {
	for _, o := range owners {
		if o.Owner == owner && o.Kind == kind {
			return true
		}
	}
	return false
}
