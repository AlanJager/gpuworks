package pluginbridge

import (
	"bytes"
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

func withRoutes(s *extsign.Spec) {
	s.Manifest.Requires = map[string]int{extapi.CapRoutes: 1}
	s.Manifest.Permissions = []string{"http:routes"}
	s.Manifest.Routes = []extapi.RouteDecl{{Method: "GET", Pattern: "/jobs", Action: s.Manifest.ID + ".jobs", Access: extapi.RouteAdmin, Ungated: true}}
}

// A package whose id is a compiled module's name is refused at install,
// before approval; one installed before the module was compiled in no
// longer runs or serves its routes after the next boot.
func TestAPackageIDACompiledModuleTakes(t *testing.T) {
	w := newWorld(t, nil)
	w.compiled = []string{"acme_backup"}
	b := w.open()
	_, err := b.Install(context.Background(), bytes.NewReader(w.pkg("acme_backup", "1.0.0", withRoutes)), InstallRequest{By: "admin"})
	if code, params, _ := codeOf(err); code != messages.ErrorPluginIDTaken || params["id"] != "acme_backup" {
		t.Fatalf("install = %v", err)
	}
	if len(b.List()) != 0 {
		t.Fatalf("a refused package is installed: %+v", b.List())
	}

	w2 := newWorld(t, nil)
	b2 := w2.open()
	install(t, b2, w2.pkg("acme_backup", "1.0.0", withRoutes), true)
	if r, _, res := b2.Routes().Match("GET", "/acme_backup/jobs"); res != MatchFound || r.Down != "" {
		t.Fatalf("routes before the module = %v %v", r, res)
	}
	w2.compiled = []string{"acme_backup"}
	b3 := w2.open()
	v := b3.List()
	if len(v) != 1 || v[0].State != StateBroken || v[0].Code != messages.ErrorPluginIDTaken {
		t.Fatalf("after the module was compiled in: %+v", v)
	}
	if _, _, res := b3.Routes().Match("GET", "/acme_backup/jobs"); res != MatchMiss {
		t.Fatalf("its routes are still served: %v", res)
	}
}
