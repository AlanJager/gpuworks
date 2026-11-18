package pluginbridge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
	"gpuworks.ai/gpuworks/pkg/pluginsdk/plugintest"
)

type seen struct {
	status       int
	code, reason string
}

// plugintest's fake host refuses what pushed state refuses with the same
// status, code and reason: a generation that is not newer, a patch, a
// schema the server does not speak. A plugin author's test of them then
// holds against a real server.
func TestPlugintestRefusesPushesAsPushedStateDoes(t *testing.T) {
	pushes := []struct{ method, body string }{
		{http.MethodPut, `{"body":{"banners":[]},"generation":1,"kind":"banners","schema":1}`},
		{http.MethodPut, `{"body":{"banners":[]},"generation":1,"kind":"banners","schema":1}`},
		{http.MethodPatch, `{"body":{"banners":[]},"generation":2,"kind":"banners","schema":1}`},
		{http.MethodPut, `{"body":{"banners":[]},"generation":3,"kind":"banners","schema":9}`},
	}

	// The real side: the bridge's state host over pushed state, for a
	// started package approved to push banners.
	w := newWorld(t, nil)
	b := w.open()
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), false)
	b.mu.Lock()
	b.pushers["acme_backup"] = identity{version: "1.0.0", pusher: pushstate.Pusher{ID: "acme_backup", Publisher: "acme",
		Grants: w.pub.Cert.Grants, Permissions: []string{"push:banners"}}}
	b.mu.Unlock()
	owner := pluginhost.Owner{ID: "acme_backup", Version: "1.0.0", Lifecycle: extapi.LifecycleResident, Generation: 1}
	real := make([]seen, len(pushes))
	for i, p := range pushes {
		push, err := extapi.DecodeStatePush([]byte(p.body))
		if err != nil {
			t.Fatal(err)
		}
		real[i].status = http.StatusOK
		if _, err := (stateHost{b}).Push(context.Background(), owner, p.method, push); err != nil {
			var e *pluginhost.Error
			if !errors.As(err, &e) {
				t.Fatalf("push %d: %v", i, err)
			}
			real[i] = seen{e.Status, e.Wire.Code, e.Wire.Reason}
		}
	}

	// The fake side, from an initialized plugin.
	cfg := pluginsdk.Config{ID: "acme_backup", Version: "1.0.0", ManifestSHA256: extapi.Digest("sha256:" + strings.Repeat("a", 64)),
		Requires: map[string]extapi.Range{"table.banners": {Min: 1, Max: 1}}}
	ready := make(chan *pluginsdk.Plugin, 1)
	cfg.Start = func(_ context.Context, p *pluginsdk.Plugin) error { ready <- p; return nil }
	h := plugintest.Start(t, cfg)
	if _, err := h.Initialize(h.Request()); err != nil {
		t.Fatal(err)
	}
	p := <-ready
	for i, push := range pushes {
		req, err := http.NewRequest(push.method, "http://gwx-host:80"+extapi.PathHostState+"banners", strings.NewReader(push.body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := p.HostClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fake := seen{status: resp.StatusCode}
		if resp.StatusCode/100 != 2 {
			we, err := extapi.DecodeWireError(raw)
			if err != nil {
				t.Fatalf("push %d: %v", i, err)
			}
			fake.code, fake.reason = we.Code, we.Reason
		}
		if fake != real[i] {
			t.Errorf("%s generation push %d: plugintest answers %+v, pushed state %+v", push.method, i, fake, real[i])
		}
	}
	if real[0].status != http.StatusOK || real[1].code == "" || real[2].code == "" || real[3].code == "" {
		t.Fatalf("the real answers do not cover the refusals: %+v", real)
	}
}

// plugintest signs a route request's principal exactly as extapi does under
// the key it hands the plugin in initialize, so a plugin's tests of
// RequirePrincipal hold against the server's signature.
func TestPlugintestSignsThePrincipalAsExtapiDoes(t *testing.T) {
	cfg := pluginsdk.Config{ID: "acme_backup", Version: "1.0.0", ManifestSHA256: extapi.Digest("sha256:" + strings.Repeat("a", 64))}
	h := plugintest.Start(t, cfg)
	key := h.Request().PrincipalKey
	if !bytes.Equal(key, h.PrincipalKey()) {
		t.Fatal("the harness signs with a key other than the one initialize hands the plugin")
	}
	p := h.Principal("acme_backup.jobs_create", "/jobs")
	access := extapi.FeatureAccess{"acme.backup": extapi.AccessReadOnly}
	req, err := http.NewRequest(http.MethodPost, "http://gwx-plugin:80"+extapi.PathRoutePrefix+"/jobs", nil)
	if err != nil {
		t.Fatal(err)
	}
	h.Sign(req, p, access)
	hdr, mac, err := extapi.SignPrincipal(key, p, access.String())
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get(extapi.HeaderPrincipal) != hdr || req.Header.Get(extapi.HeaderPrincipalMac) != mac ||
		req.Header.Get(extapi.HeaderFeatureAccess) != "acme.backup=read_only" {
		t.Fatalf("plugintest headers %v differ from extapi's %s / %s", req.Header, hdr, mac)
	}
}

// The server's signer, under the key a plugin got in initialize, signs a
// principal the SDK accepts for the route's action, carrying the caller
// and the feature access; a 4 KiB request id is clipped, not refused.
func TestServerSignedRoutesVerifyInThePlugin(t *testing.T) {
	cfg := pluginsdk.Config{ID: "acme_backup", Version: "1.0.0", ManifestSHA256: extapi.Digest("sha256:" + strings.Repeat("a", 64))}
	got := make(chan *extapi.Principal, 1)
	access := make(chan extapi.FeatureAccess, 1)
	cfg.Handler = pluginsdk.RequirePrincipal("acme_backup.jobs_create", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- pluginsdk.PrincipalFrom(r)
		access <- pluginsdk.FeatureAccessFrom(r)
	}))
	h := plugintest.Start(t, cfg)
	if _, err := h.Initialize(h.Request()); err != nil {
		t.Fatal(err)
	}
	decl := extapi.RouteDecl{Method: http.MethodPost, Pattern: "/jobs", Action: "acme_backup.jobs_create", Access: extapi.RouteAdminSession, Feature: "acme.backup"}
	r := &Route{Plugin: "acme_backup", Version: "1.0.0", Decl: decl, Timeout: 60 * time.Second}
	caller := Caller{Kind: extapi.PrincipalSession, UserID: 7, Username: "ada", Role: "admin", LocalAdmin: true,
		RequestID: strings.Repeat("r", 4096), Access: extapi.AccessReadOnly}
	req, err := http.NewRequest(http.MethodPost, "http://gwx-plugin:80"+extapi.PathRoutePrefix+"/jobs", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := signerFor(r, caller)(h.PrincipalKey(), req.Header); err != nil {
		t.Fatal(err)
	}
	resp, err := h.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("server-signed route = %d", resp.StatusCode)
	}
	p := <-got
	if p.Kind != "session" || p.UserID != 7 || p.Username != "ada" || !p.PlatformAdmin || !p.LocalAdmin ||
		len(p.Roles) != 1 || p.Roles[0] != "admin" || p.RouteAction != decl.Action || p.Route != "/jobs" || p.RequestID != strings.Repeat("r", 128) {
		t.Fatalf("principal = %+v", p)
	}
	if a := <-access; len(a) != 1 || a["acme.backup"] != extapi.AccessReadOnly {
		t.Fatalf("feature access = %v", a)
	}

	// Under another spawn's key the plugin refuses it.
	other := bytes.Repeat([]byte{9}, extapi.PrincipalKeyBytes)
	req, _ = http.NewRequest(http.MethodPost, "http://gwx-plugin:80"+extapi.PathRoutePrefix+"/jobs", nil)
	_ = signerFor(r, caller)(other, req.Header)
	resp, err = h.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("another key's signature = %d", resp.StatusCode)
	}
}
