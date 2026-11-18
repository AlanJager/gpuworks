package pluginbridge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

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
