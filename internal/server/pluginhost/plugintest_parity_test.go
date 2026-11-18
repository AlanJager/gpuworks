package pluginhost

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
	"gpuworks.ai/gpuworks/pkg/pluginsdk/plugintest"
)

// hostCall is one call a plugin makes on the host API.
type hostCall struct{ method, path, body string }

// refusalSeen is what a plugin can observe of a refusal.
type refusalSeen struct {
	status       int
	code, reason string
}

// fakeHostRefusals makes calls on plugintest's fake host, in order, from an
// initialized plugin, and returns what each answered; held is what the fake
// holds before the first call.
func fakeHostRefusals(t *testing.T, held []extapi.HeldState, calls []hostCall) []refusalSeen {
	t.Helper()
	cfg := sdkConfig("acme_backup")
	ready := make(chan *pluginsdk.Plugin, 1)
	cfg.Start = func(_ context.Context, p *pluginsdk.Plugin) error { ready <- p; return nil }
	h := plugintest.Start(t, cfg)
	h.SetHeld(held...)
	if _, err := h.Initialize(h.Request()); err != nil {
		t.Fatal(err)
	}
	p := <-ready
	out := make([]refusalSeen, len(calls))
	for i, c := range calls {
		req, err := http.NewRequest(c.method, "http://gwx-host:80"+c.path, strings.NewReader(c.body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := p.HostClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		out[i].status = resp.StatusCode
		if resp.StatusCode/100 != 2 {
			w, err := extapi.DecodeWireError(raw)
			if err != nil {
				t.Fatalf("%s %s: %v", c.method, c.path, err)
			}
			out[i].code, out[i].reason = w.Code, w.Reason
		}
	}
	return out
}

// plugintest's fake host refuses the calls the host API routes with the
// same status, code and reason, so a plugin author's test of them holds
// against a real server.
func TestPlugintestRefusesRoutesAsTheHostAPIDoes(t *testing.T) {
	calls := []hostCall{
		{http.MethodGet, "/host/v1/nothing", ""},
		{http.MethodPost, extapi.PathHostHealth, ""},
		{http.MethodGet, extapi.PathHostState + "banners", ""},
		{http.MethodDelete, extapi.PathHostState + "banners", ""},
		{http.MethodPut, extapi.PathHostState + "node_holds", `{"body":{},"generation":9,"kind":"banners","schema":1}`},
	}
	h := testHostConn(t, &fakeState{}, extapi.LifecycleResident)
	h.accept(owner())
	fake := fakeHostRefusals(t, nil, calls)
	for i, c := range calls {
		a := call(t, h, context.Background(), c.method, c.path, c.body)
		real := refusalSeen{status: a.status}
		if a.wire != nil {
			real.code, real.reason = a.wire.Code, a.wire.Reason
		}
		if real.code == "" || fake[i] != real {
			t.Errorf("%s %s: plugintest answers %+v, the host API %+v", c.method, c.path, fake[i], real)
		}
	}
}
