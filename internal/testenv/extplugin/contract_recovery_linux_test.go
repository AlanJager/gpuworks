//go:build linux

package extplugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// recoveryBudget is how long each call of the recovery surface may take
// while every plugin crashloops (I7).
const recoveryBudget = 10 * time.Second

// I7: with every installed plugin crashlooping, one of them holding every
// node and the other capping the fleet at zero cards, the local
// administrator signs in afresh, is told it is one, lists the plugins,
// overrides one node's hold so that node is schedulable, disables and
// uninstalls each, and every call answers within the budget; the disables
// run while the upgrade of a third, healthy package waits out its health
// gate, which holds up only that package.
func TestContractLocalAdminDuringCrashloop(t *testing.T) {
	s := newSite(t)
	s.upgradeGate = 30 * time.Second
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
	if code := enrollmentRefusal(t, st, "c"); code != "ACME_CARD_LIMIT" {
		t.Fatalf("an enrollment under the zero cards cap = %q; want ACME_CARD_LIMIT", code)
	}

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
	if code := enrollmentRefusal(t, st, "c"); code != "ACME_CARD_LIMIT" {
		t.Fatalf("an enrollment under the crashlooping plugin's zero cards cap = %q; want ACME_CARD_LIMIT", code)
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
	// The local administrator raises the crashlooping plugin's zero cards
	// cap and releases node a from the other's hold, and the node is
	// schedulable within the budget.
	start := time.Now()
	for what, req := range map[string]string{
		"loosen of the cards cap": `{"owner":"acme_fake","kind":"fleet_capacity","entry":"max_cards","action":"loosen","value":4,"reason":"recovering the site"}`,
		"override of node a":      fmt.Sprintf(`{"owner":"acme_other","kind":"node_holds","entry":"%d","action":"delete","reason":"recovering the site"}`, a.ID),
	} {
		var o dto.PluginOverride
		if out := timed(what, http.MethodPost, "/api/v1/plugins/overrides", req); json.Unmarshal(out, &o) != nil || o.ID == 0 {
			t.Fatalf("%s answered %s", what, out)
		}
	}
	waitFor(t, "node a schedulable", func() bool { return !heldByPlugins(t, st, a.UUID) })
	if took := time.Since(start); took >= recoveryBudget {
		t.Fatalf("node a was released after %s; want within %s", took, recoveryBudget)
	}
	if !heldByPlugins(t, st, b.UUID) {
		t.Fatal("the override of node a released node b")
	}
	sv.install(s.fakePackage("acme_third", "1.0.0"), true)
	sv.waitPlugin("acme_third", "running", func(p dto.Plugin) bool { return p.State == "running" })
	third := s.fakePackage("acme_third", "2.0.0")
	status, out := sv.do(http.MethodPost, "/api/v1/plugins/acme_third/upgrade", bytes.NewReader(third))
	var approval struct {
		Params map[string]any `json:"params"`
	}
	if status != http.StatusConflict || json.Unmarshal(out, &approval) != nil {
		t.Fatalf("upgrade of acme_third without approval = %d %s", status, out)
	}
	upgraded := make(chan int, 1)
	go func() {
		status, _ := sv.do(http.MethodPost, "/api/v1/plugins/acme_third/upgrade?approve="+url.QueryEscape(fmt.Sprint(approval.Params["approve_hash"])),
			bytes.NewReader(third))
		upgraded <- status
	}()
	sv.waitPlugin("acme_third", "in its health gate", func(p dto.Plugin) bool { return p.Version == "2.0.0" && p.State == "running" })
	for _, id := range []string{"acme_fake", "acme_other"} {
		var p dto.Plugin
		if out := timed("disable "+id, http.MethodPost, "/api/v1/plugins/"+id+"/disable", ""); json.Unmarshal(out, &p) != nil || p.State != "disabled" {
			t.Fatalf("disable %s answered %s", id, out)
		}
	}
	for _, id := range []string{"acme_fake", "acme_other"} {
		var im dto.PluginImpact
		if out := timed("the uninstall impact of "+id, http.MethodGet, "/api/v1/plugins/"+id+"/impact", ""); json.Unmarshal(out, &im) != nil || im.Confirm == "" {
			t.Fatalf("impact of %s answered %s", id, out)
		}
		timed("uninstall "+id, http.MethodDelete, "/api/v1/plugins/"+id+"?confirm="+url.QueryEscape(im.Confirm), "")
	}
	select {
	case status := <-upgraded:
		t.Fatalf("the upgrade of acme_third answered %d before its gate ended", status)
	default:
	}
	if status := <-upgraded; status != http.StatusOK {
		t.Fatalf("upgrade of acme_third = %d", status)
	}
	for _, id := range []string{"acme_fake", "acme_other"} {
		if p := sv.plugin(id); p.State != "absent" {
			t.Fatalf("%s after uninstall = %+v", id, p)
		}
	}
	waitFor(t, "every hold released", func() bool { return !heldByPlugins(t, st, a.UUID) && !heldByPlugins(t, st, b.UUID) })
	if code := enrollmentRefusal(t, st, "c"); code != "" {
		t.Fatalf("an enrollment after uninstall was refused with %q; the cards cap must be lifted", code)
	}
}

// enrollmentRefusal tries to enroll a node and answers the refusal's code,
// or "" when the node joined: the observable effect of the fleet cards cap.
func enrollmentRefusal(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	_, _, err := st.CreateNodePending(name)
	if err == nil {
		return ""
	}
	var refusal *messages.ProductError
	if !errors.Is(err, store.ErrNodeEnrollmentRefused) || !errors.As(err, &refusal) {
		t.Fatalf("enrolling %s = %v; want a refusal or success", name, err)
	}
	return refusal.Code()
}
