package pluginsdk_test

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

// plugintest's fake host refuses host calls with the server's bytes: the
// same literals pin the server's refusals in its own host API test.
func TestPlugintestHostCallRefusalsOnTheWire(t *testing.T) {
	_, granted := startAuthority(t, grantAll)
	_, plain := startAuthority(t, nil)
	cfg := baseConfig()
	started := make(chan *pluginsdk.Plugin, 1)
	cfg.Start = func(_ context.Context, p *pluginsdk.Plugin) error { started <- p; return nil }
	h := plugintest.Start(t, cfg)
	if _, err := h.Initialize(h.Request()); err != nil {
		t.Fatal(err)
	}
	noCap := <-started
	for _, tc := range []struct {
		name         string
		p            *pluginsdk.Plugin
		method, path string
		body         string
		status       int
		want         string
	}{
		{"role", plain, http.MethodPost, extapi.PathHostClockCorrect, `{"reason":"x","to":"2027-01-15T00:00:00Z"}`, 403,
			`{"code":"PLUGIN_ROLE_REQUIRED","message":"the plugin needs the clock_authority role","params":{"id":"acme_backup","role":"clock_authority"},"reason":"permission_denied"}`},
		{"permission", plain, http.MethodPost, extapi.PathHostDeriveKey, `{"label":"usb.challenge","length":32}`, 403,
			`{"code":"PLUGIN_HOST_NOT_PERMITTED","message":"the plugin was not granted crypto:derive_key","params":{"id":"acme_backup","permission":"crypto:derive_key"},"reason":"permission_denied"}`},
		{"argument", granted, http.MethodPost, extapi.PathHostClockCorrect, `{"reason":"x"}`, 400,
			`{"code":"PLUGIN_HOST_ARGUMENT_INVALID","message":"invalid to","params":{"field":"to","id":"acme_backup"},"reason":"invalid_argument"}`},
		{"kind", granted, http.MethodGet, extapi.PathHostState + "foo", "", 400,
			`{"code":"PLUGIN_HOST_ARGUMENT_INVALID","message":"the path names no table kind","params":{"field":"kind","id":"acme_backup"},"reason":"invalid_argument"}`},
		{"capability", noCap, http.MethodGet, extapi.PathHostMeta, "", 412,
			`{"code":"PLUGIN_CAPABILITY_NOT_OFFERED","message":"the plugin did not negotiate host.clock at its handshake","params":{"capability":"host.clock","id":"acme_backup","version":1},"reason":"failed_precondition"}`},
		{"size", granted, http.MethodPost, extapi.PathHostClockCorrect, `{"reason":"` + strings.Repeat("x", extapi.MaxHostCallBytes) + `"}`, 413,
			`{"code":"REQUEST_TOO_LARGE","message":"the request is larger than the server reads","params":{"max_bytes":4096},"reason":"invalid_argument"}`},
	} {
		req, err := http.NewRequest(tc.method, "http://gwx-host:80"+tc.path, strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := tc.p.HostClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if got := strings.TrimSpace(string(raw)); resp.StatusCode != tc.status || got != tc.want {
			t.Errorf("%s: %d\n%s\nwant %d\n%s", tc.name, resp.StatusCode, got, tc.status, tc.want)
		}
	}
}
