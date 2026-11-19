package pluginhost

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
	"gpuworks.ai/gpuworks/pkg/pluginsdk/plugintest"
)

// hostCallSeen uses the shared canonical refusal observed by a plugin.
func hostCallSeen(t *testing.T, status int, raw []byte) refusalSeen {
	t.Helper()
	var wire *extapi.WireError
	if status/100 != 2 {
		var err error
		wire, err = extapi.DecodeWireError(raw)
		if err != nil {
			t.Fatal(err)
		}
	}
	return seen(t, status, wire)
}

// plugintestCalls makes calls on plugintest's fake host from a plugin that
// uses uses and was granted grant, and returns what each answered with the
// owner the real host API would bind for the same handshake.
func plugintestCalls(t *testing.T, uses map[string]extapi.Range, grant extapi.PluginGrant, calls []hostCall) ([]refusalSeen, Owner) {
	t.Helper()
	cfg := sdkConfig("acme_quota")
	cfg.Uses = uses
	ready := make(chan *pluginsdk.Plugin, 1)
	cfg.Start = func(_ context.Context, p *pluginsdk.Plugin) error { ready <- p; return nil }
	h := plugintest.Start(t, cfg)
	req := h.Request()
	req.Plugin.PermissionsGranted, req.Plugin.RolesGranted = grant.PermissionsGranted, grant.RolesGranted
	resp, err := h.Initialize(req)
	if err != nil {
		t.Fatal(err)
	}
	p := <-ready
	out := make([]refusalSeen, len(calls))
	for i, c := range calls {
		r, err := http.NewRequest(c.method, "http://gwx-host:80"+c.path, strings.NewReader(c.body))
		if err != nil {
			t.Fatal(err)
		}
		res, err := p.HostClient().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		out[i] = hostCallSeen(t, res.StatusCode, raw)
	}
	grant.ID, grant.Version = cfg.ID, cfg.Version
	return out, Owner{ID: cfg.ID, Version: cfg.Version, Lifecycle: extapi.LifecycleResident, Grant: grant, Use: resp.Use, Generation: 1}
}

// plugintest's fake host answers the clock, ready, own-state and key calls
// with the same status, code, reason and params as the host API, so a
// plugin author's test of their refusals holds against a real server.
func TestPlugintestAnswersTheHostCallsAsTheHostAPIDoes(t *testing.T) {
	all := map[string]extapi.Range{extapi.CapHostClock: {Min: 1, Max: 1}, extapi.CapHostStateRead: {Min: 1, Max: 1}, extapi.CapHostDeriveKey: {Min: 1, Max: 1}}
	granted := extapi.PluginGrant{PermissionsGranted: []string{"crypto:derive_key"}, RolesGranted: []string{"clock_authority"}}
	big := `{"label":"` + strings.Repeat("a", extapi.MaxHostCallBytes) + `","length":32}`
	for _, tc := range []struct {
		name  string
		uses  map[string]extapi.Range
		grant extapi.PluginGrant
		calls []hostCall
	}{
		{"nothing negotiated or granted", nil, extapi.PluginGrant{}, []hostCall{
			{http.MethodGet, extapi.PathHostMeta, ""},
			{http.MethodPost, extapi.PathHostClockCorrect, correctBody},
			{http.MethodGet, extapi.PathHostState + "banners", ""},
			{http.MethodPost, extapi.PathHostDeriveKey, `{"label":"usb.challenge","length":32}`},
			{http.MethodPost, extapi.PathHostReady, ""},
			{http.MethodGet, extapi.PathHostReady, ""},
			{http.MethodPut, extapi.PathHostReady, ""},
			{http.MethodPost, extapi.PathHostMeta, ""},
		}},
		{"granted but not negotiated", nil, granted, []hostCall{
			{http.MethodPost, extapi.PathHostClockCorrect, correctBody},
			{http.MethodPost, extapi.PathHostDeriveKey, `{"label":"usb.challenge","length":32}`},
		}},
		{"negotiated and granted", all, granted, []hostCall{
			{http.MethodPost, extapi.PathHostClockCorrect, `{"reason":"x"}`},
			{http.MethodPost, extapi.PathHostClockCorrect, `{"reason":"x","to":"2027-01-15"}`},
			{http.MethodPost, extapi.PathHostDeriveKey, `{"label":"USB","length":32}`},
			{http.MethodPost, extapi.PathHostDeriveKey, `{"label":"usb","length":8}`},
			{http.MethodPost, extapi.PathHostDeriveKey, `{"label":"usb","length":32,"purpose":"x"}`},
			{http.MethodPost, extapi.PathHostDeriveKey, big},
			{http.MethodPost, extapi.PathHostDeriveKey, `{"label":"usb.challenge","length":32}`},
			{http.MethodGet, extapi.PathHostState + "Banners", ""},
			{http.MethodGet, extapi.PathHostState + "banners", ""},
			{http.MethodGet, extapi.PathHostMeta, ""},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake, o := plugintestCalls(t, tc.uses, tc.grant, tc.calls)
			h := servicesConn(t, &fakeServices{}, o)
			for i, c := range tc.calls {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, strings.NewReader(c.body)))
				real := hostCallSeen(t, rec.Code, rec.Body.Bytes())
				if fake[i] != real {
					t.Errorf("%s %s %.40s: plugintest answers %+v, the host API %+v", c.method, c.path, c.body, fake[i], real)
				}
			}
		})
	}
}
