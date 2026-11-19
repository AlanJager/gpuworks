package pluginhost

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
	"gpuworks.ai/gpuworks/pkg/pluginsdk/plugintest"
)

// hostCall is one call a plugin makes on the host API.
type hostCall struct{ method, path, body string }

// refusalSeen is what a plugin can observe of a refusal; params is the
// canonical JSON of its parameters.
type refusalSeen struct {
	status               int
	code, reason, params string
}

func seen(t *testing.T, status int, w *extapi.WireError) refusalSeen {
	t.Helper()
	s := refusalSeen{status: status}
	if w == nil {
		return s
	}
	params, err := json.Marshal(w.Params)
	if err != nil {
		t.Fatal(err)
	}
	s.code, s.reason, s.params = w.Code, w.Reason, string(params)
	return s
}

// fakeHostRefusals makes calls on plugintest's fake host, in order, from an
// initialized plugin, and returns what each answered; held is what the fake
// holds before the first call.
func fakeHostRefusals(t *testing.T, held []extapi.HeldState, calls []hostCall) []refusalSeen {
	t.Helper()
	return fakeHostAnswers(t, held, nil, nil, calls)
}

// fakeHostAnswers is fakeHostRefusals for a plugin granted perms that also
// uses the capabilities uses.
func fakeHostAnswers(t *testing.T, held []extapi.HeldState, perms []string, uses map[string]extapi.Range, calls []hostCall) []refusalSeen {
	t.Helper()
	cfg := sdkConfig("acme_backup")
	cfg.Uses = uses
	ready := make(chan *pluginsdk.Plugin, 1)
	cfg.Start = func(_ context.Context, p *pluginsdk.Plugin) error { ready <- p; return nil }
	h := plugintest.Start(t, cfg)
	h.SetHeld(held...)
	req := h.Request()
	req.Plugin.PermissionsGranted = perms
	if _, err := h.Initialize(req); err != nil {
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
		var w *extapi.WireError
		if resp.StatusCode/100 != 2 {
			if w, err = extapi.DecodeWireError(raw); err != nil {
				t.Fatalf("%s %s: %v", c.method, c.path, err)
			}
		}
		out[i] = seen(t, resp.StatusCode, w)
	}
	return out
}

// plugintest's fake host refuses the calls the host API routes with the
// same status, code, reason and parameters, so a plugin author's test of them holds
// against a real server.
func TestPlugintestRefusesRoutesAsTheHostAPIDoes(t *testing.T) {
	calls := []hostCall{
		{http.MethodGet, "/host/v1/nothing", ""},
		{http.MethodPost, extapi.PathHostHealth, ""},
		{http.MethodDelete, extapi.PathHostState + "banners", ""},
		{http.MethodPut, extapi.PathHostState + "node_holds", `{"body":{},"generation":9,"kind":"banners","schema":1}`},
	}
	h := testHostConn(t, &fakeState{}, extapi.LifecycleResident)
	h.accept(owner())
	fake := fakeHostRefusals(t, nil, calls)
	for i, c := range calls {
		a := call(t, h, context.Background(), c.method, c.path, c.body)
		real := seen(t, a.status, a.wire)
		if real.code == "" || fake[i] != real {
			t.Errorf("%s %s: plugintest answers %+v, the host API %+v", c.method, c.path, fake[i], real)
		}
	}
}

// plugintest's fake host checks the node reads as the host API does: the
// permission, then the negotiated capability, then the arguments, each
// refused with the same status, code, reason and parameters.
func TestPlugintestRefusesReadsAsTheHostAPIDoes(t *testing.T) {
	reads := map[string]extapi.Range{extapi.CapHostNodeIdentity: {Min: 1, Max: 1}, extapi.CapHostNodes: {Min: 1, Max: 1}}
	argCalls := []hostCall{
		{http.MethodPost, extapi.PathHostNodeIdentity, `{"node_ids":[1],"salt":""}`},
		{http.MethodPost, extapi.PathHostNodeIdentity, `{"node_ids":[1],"salt":"` + strings.Repeat("s", extapi.MaxNodeIdentitySalt+1) + `"}`},
		{http.MethodPost, extapi.PathHostNodeIdentity, `{"node_ids":[],"salt":"s"}`},
		{http.MethodPost, extapi.PathHostNodeIdentity, `{"node_ids":[3,3],"salt":"s"}`},
		{http.MethodPost, extapi.PathHostNodeIdentity, `{"node_ids":[1],"salt":"s","by":"x"}`},
		{http.MethodPost, extapi.PathHostNodeIdentity, `{`},
		{http.MethodPost, extapi.PathHostNodeIdentity, `{"node_ids":[1],"salt":"` + strings.Repeat("s", extapi.MaxNodeIdentityRequestBytes) + `"}`},
		{http.MethodGet, extapi.PathHostNodeIdentity, ""},
		{http.MethodPut, extapi.PathHostNodes, ""},
		{http.MethodGet, extapi.PathHostNodes + "?cursor=abc", ""},
		{http.MethodGet, extapi.PathHostNodes + "?cursor=0", ""},
		{http.MethodGet, extapi.PathHostNodes + "?limit=501", ""},
		{http.MethodGet, extapi.PathHostNodes + "?after=3", ""},
	}
	gateCalls := []hostCall{
		{http.MethodPost, extapi.PathHostNodeIdentity, `{"node_ids":[1],"salt":"s"}`},
		{http.MethodGet, extapi.PathHostNodes, ""},
	}
	all := []string{"read:node_identity", "read:node_inventory"}
	cases := []struct {
		name  string
		perms []string
		uses  map[string]extapi.Range
		calls []hostCall
	}{
		{"arguments", all, reads, argCalls},
		{"not permitted", nil, reads, gateCalls},
		{"not negotiated", all, nil, gateCalls},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := readerOwner(extapi.LifecycleResident, c.perms...)
			o.Use = map[string]int{"table.banners": 1}
			for name := range c.uses {
				o.Use[name] = 1
			}
			h := readerConn(t, &fakeReads{n: 3}, o)
			h.pages = newTokenBucket(1000, 1000, time.Now)
			fake := fakeHostAnswers(t, nil, c.perms, c.uses, c.calls)
			for i, call := range c.calls {
				a := readHost(t, h, call.method, call.path, call.body)
				real := seen(t, a.status, a.wire)
				if real.code == "" || fake[i] != real {
					t.Errorf("%s %s %s: plugintest answers %+v, the host API %+v", call.method, call.path, call.body, fake[i], real)
				}
			}
		})
	}
}
