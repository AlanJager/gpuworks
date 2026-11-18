//go:build linux

package extplugin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// recoveryBudget is how long each call of the recovery surface may take
// while every plugin crashloops (I7).
const recoveryBudget = 10 * time.Second

// I7: with every installed plugin crashlooping, one of them holding every
// node and the other capping the fleet at zero cards, the local
// administrator signs in afresh, is told it is one, lists the plugins,
// disables and uninstalls each, and every call answers within the budget.
func TestContractLocalAdminDuringCrashloop(t *testing.T) {
	s := newSite(t)
	sv := s.boot()
	st := sv.a.Store()
	a, b := node(t, st, "a", 2), node(t, st, "b", 2)

	zeroCap := strings.Replace(capacityBody, `"steps":[{"max_cards":4}]`, `"steps":[{"max_cards":0}]`, 1)
	if zeroCap == capacityBody {
		t.Fatal("the capacity body no longer has the step this test rewrites")
	}
	holdAll := fmt.Sprintf(`{"nodes":{"%d":{"code":"ACME_MAINTENANCE"},"%d":{"code":"ACME_MAINTENANCE"}}}`, a.ID, b.ID)
	s.args("acme_fake", "--requires", requiresAll(), "--push", "fleet_capacity="+zeroCap)
	s.args("acme_other", "--requires", requiresAll(), "--push", "node_holds="+holdAll)
	sv.install(s.fakePackage("acme_fake", "1.0.0"), true)
	sv.install(s.fakePackage("acme_other", "1.0.0"), true)
	for _, id := range []string{"acme_fake", "acme_other"} {
		if r := s.waitResults(id, 1)[0]; !r.OK {
			t.Fatalf("%s push = %+v", id, r)
		}
	}
	waitFor(t, "every node held", func() bool { return heldByPlugins(t, st, a.UUID) && heldByPlugins(t, st, b.UUID) })

	// Both plugins now die at every start, from the next boot on.
	sv.stop()
	for _, id := range []string{"acme_fake", "acme_other"} {
		s.args(id, "--exit-at-start", "3")
	}
	sv = s.boot()
	st = sv.a.Store()
	for _, id := range []string{"acme_fake", "acme_other"} {
		sv.waitPlugin(id, "in crashloop", func(p dto.Plugin) bool { return p.State == "crashloop" })
	}
	if !heldByPlugins(t, st, a.UUID) || !heldByPlugins(t, st, b.UUID) {
		t.Fatal("the crashlooping plugins' holds were released")
	}

	timed := func(what string, method, path, body string) []byte {
		t.Helper()
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		start := time.Now()
		status, out := sv.do(method, path, reader)
		if took := time.Since(start); status < 200 || status > 299 || took >= recoveryBudget {
			t.Fatalf("%s = %d %s after %s; want 2xx within %s", what, status, out, took, recoveryBudget)
		}
		return out
	}

	sv.token = ""
	var login struct {
		Token string `json:"token"`
	}
	out := timed("a fresh login", http.MethodPost, "/api/v1/auth/login", `{"username":"admin","password":"`+adminPassword+`"}`)
	if json.Unmarshal(out, &login) != nil || login.Token == "" {
		t.Fatalf("login answered %s", out)
	}
	sv.token = login.Token
	var me struct {
		Role       string `json:"role"`
		LocalAdmin bool   `json:"local_admin"`
	}
	if out := timed("/auth/me", http.MethodGet, "/api/v1/auth/me", ""); json.Unmarshal(out, &me) != nil || me.Role != "admin" || !me.LocalAdmin {
		t.Fatalf("/auth/me = %s; want the local administrator", out)
	}
	var list dto.PluginList
	if out := timed("the plugin list", http.MethodGet, "/api/v1/plugins", ""); json.Unmarshal(out, &list) != nil || len(list.Items) != 2 {
		t.Fatalf("plugin list = %s", out)
	}
	for _, id := range []string{"acme_fake", "acme_other"} {
		timed("disable "+id, http.MethodPost, "/api/v1/plugins/"+id+"/disable", "")
	}
	for _, id := range []string{"acme_fake", "acme_other"} {
		timed("uninstall "+id, http.MethodDelete, "/api/v1/plugins/"+id, "")
	}
	if p := sv.plugin("acme_fake"); p.State != "absent" {
		t.Fatalf("acme_fake after uninstall = %+v", p)
	}
	waitFor(t, "every hold released", func() bool { return !heldByPlugins(t, st, a.UUID) && !heldByPlugins(t, st, b.UUID) })
}
