package app

import (
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/pluginbridge"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// The API's names for the plugin route wire are pkg/extapi's.
func TestPluginRouteWireNamesMatchExtapi(t *testing.T) {
	if api.PluginProductErrorHeader != extapi.HeaderProductError {
		t.Fatalf("api marks product errors with %q, extapi with %q", api.PluginProductErrorHeader, extapi.HeaderProductError)
	}
	for kind, want := range map[string]string{"session": extapi.PrincipalSession, "token": extapi.PrincipalToken, "public": extapi.PrincipalPublic} {
		if got := principalKind(kind); got != want {
			t.Errorf("principalKind(%s) = %s", kind, got)
		}
	}
}

// A route reaches the API with every declared rule and its filled-in
// limits.
func TestDynamicRouteCarriesTheDeclaration(t *testing.T) {
	r := pluginbridge.Route{Plugin: "acme_sso", Version: "2.0.0", MaxBody: 4096, Timeout: 9 * time.Second, Decl: extapi.RouteDecl{
		Method: "POST", Pattern: "/acs", Action: "acme_sso.acs", Access: extapi.RoutePublic, Ungated: true, Cookies: true, HTML: true,
		Rate: &extapi.RouteRate{PerMinute: 30, Burst: 5}}}
	d := dynamicRoute(r)
	if d.Plugin != "acme_sso" || d.Version != "2.0.0" || d.Method != "POST" || d.Pattern != "/acs" || d.Action != "acme_sso.acs" ||
		d.Access != api.DynamicPublic || d.Feature != "" || !d.Cookies || !d.HTML || d.MaxBody != 4096 || d.Timeout != 9*time.Second ||
		d.RatePerMinute != 30 || d.RateBurst != 5 || d.Ref.(*pluginbridge.Route).Decl.Action != "acme_sso.acs" {
		t.Fatalf("dynamic route = %+v", d)
	}
	for access, want := range map[string]api.DynamicAccess{extapi.RouteAdmin: api.DynamicAdmin, extapi.RouteAdminSession: api.DynamicAdminSession,
		extapi.RouteMember: api.DynamicMember, extapi.RoutePublic: api.DynamicPublic} {
		if got := dynamicAccess(access); got != want {
			t.Errorf("dynamicAccess(%s) = %v", access, got)
		}
	}
}

// A plugin's error body passes only as a valid gwx/1 error, with its code,
// params and fallback.
func TestDecodePluginError(t *testing.T) {
	pe, err := decodePluginError([]byte(`{"code":"ACME_BUSY","message":"busy","params":{"n":1},"reason":"conflict"}`))
	if err != nil || pe.Code() != "ACME_BUSY" || fmt.Sprint(pe.Params()["n"]) != "1" || pe.Error() != "busy" {
		t.Fatalf("valid = %v %v", pe, err)
	}
	for _, body := range []string{`{"code":"ACME_BUSY","message":"busy"}`, `{"code":"lower","message":"x","reason":"conflict"}`, `{"code":"A_B","message":"x","reason":"conflict","extra":1}`, `not json`} {
		if _, err := decodePluginError([]byte(body)); err == nil {
			t.Errorf("%s decoded", body)
		}
	}
}

type namedAPI string

func (n namedAPI) Name() string     { return string(n) }
func (namedAPI) Routes(api.Mounter) {}

// No package may take a compiled module's name or an extension's
// namespace.
func TestCompiledNames(t *testing.T) {
	got := compiledNames([]Module{namedModule("ops"), namedModule("audit")}, []api.Extension{namedAPI("ops"), namedAPI("acme_tools")})
	if len(got) != 3 || got[0] != "acme_tools" || got[1] != "audit" || got[2] != "ops" {
		t.Fatalf("compiled names = %v", got)
	}
}
