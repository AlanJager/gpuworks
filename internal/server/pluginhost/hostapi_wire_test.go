package pluginhost

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// The host API's refusals are pinned byte for byte: code, message, the
// exact params and the reason, as a plugin reads them off the wire.
func TestHostAPIRefusalsOnTheWire(t *testing.T) {
	noRole := authority()
	noRole.Grant.RolesGranted = []string{"availability_authority"}
	noPerm := authority()
	noPerm.Grant.PermissionsGranted = nil
	noCap := authority()
	noCap.Use = map[string]int{}
	for _, tc := range []struct {
		name         string
		owner        Owner
		method, path string
		body         string
		status       int
		want         string
	}{
		{"role", noRole, http.MethodPost, extapi.PathHostClockCorrect, correctBody, 403, `{"code":"PLUGIN_ROLE_REQUIRED","message":"the plugin needs the clock_authority role","params":{"id":"acme_quota","role":"clock_authority"},"reason":"permission_denied"}`},
		{"permission", noPerm, http.MethodPost, extapi.PathHostDeriveKey, `{"label":"usb.challenge","length":32}`, 403, `{"code":"PLUGIN_HOST_NOT_PERMITTED","message":"the plugin was not granted crypto:derive_key","params":{"id":"acme_quota","permission":"crypto:derive_key"},"reason":"permission_denied"}`},
		{"argument", authority(), http.MethodPost, extapi.PathHostClockCorrect, `{"reason":"x"}`, 400, `{"code":"PLUGIN_HOST_ARGUMENT_INVALID","message":"invalid to","params":{"field":"to","id":"acme_quota"},"reason":"invalid_argument"}`},
		{"kind", authority(), http.MethodGet, extapi.PathHostState + "foo", "", 400, `{"code":"PLUGIN_HOST_ARGUMENT_INVALID","message":"the path names no table kind","params":{"field":"kind","id":"acme_quota"},"reason":"invalid_argument"}`},
		{"capability", noCap, http.MethodGet, extapi.PathHostMeta, "", 412, `{"code":"PLUGIN_CAPABILITY_NOT_OFFERED","message":"the plugin did not negotiate host.clock at its handshake","params":{"capability":"host.clock","id":"acme_quota","version":1},"reason":"failed_precondition"}`},
		{"size", authority(), http.MethodPost, extapi.PathHostClockCorrect, `{"reason":"` + strings.Repeat("x", extapi.MaxHostCallBytes) + `"}`, 413, `{"code":"REQUEST_TOO_LARGE","message":"the request is larger than the server reads","params":{"max_bytes":4096},"reason":"invalid_argument"}`},
	} {
		h := servicesConn(t, &fakeServices{}, tc.owner)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if got := strings.TrimSpace(rec.Body.String()); rec.Code != tc.status || got != tc.want {
			t.Errorf("%s: %d\n%s\nwant %d\n%s", tc.name, rec.Code, got, tc.status, tc.want)
		}
	}
}

func TestNodeIdentitySizeRefusalOnTheWire(t *testing.T) {
	h := readerConn(t, &fakeReads{}, readerOwner(extapi.LifecycleResident, readPermissions...))
	rec := httptest.NewRecorder()
	body := `{"node_ids":[1],"salt":"` + strings.Repeat("s", extapi.MaxNodeIdentityRequestBytes) + `"}`
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/host/v1/node-identity", strings.NewReader(body)))
	want := `{"code":"REQUEST_TOO_LARGE","message":"the request is larger than the server reads","params":{"max_bytes":32768},"reason":"invalid_argument"}`
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusRequestEntityTooLarge || got != want {
		t.Fatalf("%d\n%s\nwant 413\n%s", rec.Code, got, want)
	}
}
