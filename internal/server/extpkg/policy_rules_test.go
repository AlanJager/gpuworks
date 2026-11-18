package extpkg_test

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

func routeDecl(pattern, access string) extapi.RouteDecl {
	return extapi.RouteDecl{Method: "GET", Pattern: pattern, Action: "acme_backup.read", Access: access, Ungated: true}
}

// The server's own policy refuses what the schema allows but this server
// will not run, on install and on every recheck alike.
func TestVerifyAppliesTheServerPolicy(t *testing.T) {
	cases := []struct {
		name   string
		edit   func(*extsign.Spec)
		offer  map[string]int // nil keeps the test verifier's offer
		code   string
		params map[string]any
	}{
		{name: "on_demand pushing state", edit: func(s *extsign.Spec) {
			s.Manifest.Runtime.Lifecycle = extapi.LifecycleOnDemand
			s.Manifest.Permissions = []string{"push:banners"}
			s.Manifest.Uses = map[string]int{"table.banners": 1}
		}, code: messages.ErrorPluginStateOnDemand, params: map[string]any{"id": "acme_backup", "permission": "push:banners"}},
		{name: "on_demand declaring a feature", edit: func(s *extsign.Spec) {
			s.Manifest.Runtime.Lifecycle = extapi.LifecycleOnDemand
			s.Manifest.Features = []extapi.FeatureDecl{{Key: "acme.backup", Presentation: extapi.PresentationPromote, Gate: extapi.GateSelf}}
		}, code: messages.ErrorPluginStateOnDemand, params: map[string]any{"id": "acme_backup", "feature": "acme.backup"}},
		{name: "routes without http:routes", edit: func(s *extsign.Spec) {
			s.Manifest.Requires = map[string]int{"routes": 1}
			s.Manifest.Routes = []extapi.RouteDecl{routeDecl("/jobs", extapi.RouteAdmin)}
		}, code: messages.ErrorPluginPolicyRefused, params: map[string]any{"rule": "http:routes", "value": "routes.0"}},
		{name: "public route without http:public", edit: func(s *extsign.Spec) {
			s.Manifest.Requires = map[string]int{"routes": 1}
			s.Manifest.Permissions = []string{"http:routes"}
			s.Manifest.Routes = []extapi.RouteDecl{routeDecl("/jobs", extapi.RouteAdmin), routeDecl("/hooks", extapi.RoutePublic)}
		}, code: messages.ErrorPluginPolicyRefused, params: map[string]any{"rule": "http:public", "value": "routes.1"}},
		{name: "read surface this server does not offer", offer: extpkg.Capabilities(), edit: func(s *extsign.Spec) {
			s.Manifest.Permissions = []string{"read:node_identity"}
			s.Manifest.Requires = map[string]int{"host.node_identity": 1}
		}, code: messages.ErrorPluginIncompatible, params: map[string]any{"capability": "host.node_identity", "offered": 0}},
		{name: "websocket route on a server without upgrades", edit: func(s *extsign.Spec) {
			s.Manifest.Permissions = []string{"http:routes"}
			s.Manifest.Requires = map[string]int{"routes": 1, "routes.websocket": 1}
			r := routeDecl("/watch", extapi.RouteMember)
			r.WebSocket = true
			s.Manifest.Routes = []extapi.RouteDecl{r}
		}, code: messages.ErrorPluginIncompatible, params: map[string]any{"capability": "routes.websocket"}},
	}
	for _, c := range cases {
		for _, mode := range []extpkg.Mode{extpkg.Install, extpkg.Recheck} {
			t.Run(c.name, func(t *testing.T) {
				w := newWorld(t)
				spec := extsign.Minimal("acme", "acme_backup", "1.0.0")
				c.edit(&spec)
				v := w.verifier()
				if c.offer != nil {
					v.Offered = c.offer
				}
				_, err := v.Verify(contents(t, extsign.Build(t, w.pub, spec)), mode)
				code, params := codeOf(err)
				if code != c.code {
					t.Fatalf("mode %d: err = %v (%s), want %s", mode, err, code, c.code)
				}
				for k, want := range c.params {
					if params[k] != want {
						t.Fatalf("param %s = %#v, want %#v (all %v)", k, params[k], want, params)
					}
				}
			})
		}
	}
}

// What the policy refuses in one shape it accepts in the shape it asks for.
func TestVerifyAcceptsWhatTheServerPolicyAsksFor(t *testing.T) {
	for name, edit := range map[string]func(*extsign.Spec){
		"resident pushing state": func(s *extsign.Spec) {
			s.Manifest.Permissions = []string{"push:banners"}
			s.Manifest.Uses = map[string]int{"table.banners": 1}
			s.Manifest.Features = []extapi.FeatureDecl{{Key: "acme.backup", Presentation: extapi.PresentationPromote, Gate: extapi.GateSelf}}
		},
		"on_demand with routes only": func(s *extsign.Spec) {
			s.Manifest.Runtime.Lifecycle = extapi.LifecycleOnDemand
			s.Manifest.Requires = map[string]int{"routes": 1}
			s.Manifest.Permissions = []string{"http:public", "http:routes"}
			s.Manifest.Routes = []extapi.RouteDecl{routeDecl("/jobs", extapi.RouteMember), routeDecl("/hooks", extapi.RoutePublic)}
		},
		"optional read surface this server does not offer": func(s *extsign.Spec) {
			s.Manifest.Permissions = []string{"read:node_identity"}
			s.Manifest.Uses = map[string]int{"host.node_identity": 1}
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			spec := extsign.Minimal("acme", "acme_backup", "1.0.0")
			edit(&spec)
			if _, err := w.verifier().Verify(contents(t, extsign.Build(t, w.pub, spec)), extpkg.Install); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The approval of a package that existed before the vocabulary closed is
// the same digest, so an upgrade of such a package asks for nothing new.
func TestApprovalHashOfAResidentPackageIsUnchanged(t *testing.T) {
	v := &extpkg.Verified{
		Manifest: &extapi.Manifest{Permissions: []string{"http:routes", "push:banners", "push:fleet_capacity", "read:fleet"},
			Roles:     []string{"clock_authority"},
			Runtime:   &extapi.RuntimeDecl{Lifecycle: extapi.LifecycleResident, Devices: []string{"/dev/bus/usb"}},
			HostSetup: []extapi.HostSetupStep{{Type: extapi.HostSetupUSBDevice, VendorID: "096e", ProductID: "0201"}}},
		Grants: extapi.CertGrants{MinIsolation: extapi.IsolationLandlock},
	}
	got, err := extpkg.ApprovalHash(v)
	if err != nil {
		t.Fatal(err)
	}
	// Computed by this test on the code before the vocabulary closed.
	const want = "sha256:57dbc4a022a04759f9f69734363042dc9b68387446c15373753d1537270c97db"
	if string(got) != want {
		t.Fatalf("approval hash = %s, want %s", got, want)
	}
}
