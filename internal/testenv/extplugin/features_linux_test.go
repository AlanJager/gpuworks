//go:build linux

package extplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// opsModule is a module compiled into the server that declares one feature
// key and provides the feature source, answering full.
type opsModule struct{ key featuregate.Key }

func (opsModule) Name() string { return "ops" }

func (m opsModule) Init(_ context.Context, h *app.Host) error {
	h.DeclareFeatures(featuregate.Feature{Key: m.key})
	return h.ProvideFeatures(fullSource{})
}

type fullSource struct{}

func (fullSource) Availability(featuregate.Key) featuregate.Availability {
	return featuregate.Availability{Access: featuregate.Full}
}

func featureKeys(m dto.ConsoleManifest) []string {
	out := make([]string, len(m.Features))
	for i, f := range m.Features {
		out[i] = f.Key
	}
	return out
}

// A server with a compiled module that provides features runs plugin
// packages that declare their own: a package installed while it runs is
// in the console manifest at once, sorted with the module's key, the
// server boots again with it installed, and an uninstall withdraws it.
func TestContractRuntimeFeatureDeclarations(t *testing.T) {
	s := newSite(t)
	s.modules = []app.Module{opsModule{key: "ops.audit"}}
	sv := s.boot()
	if got := featureKeys(sv.manifest()); !slices.Equal(got, []string{"ops.audit"}) {
		t.Fatalf("features before any install = %v", got)
	}
	sv.install(s.fakePackage("acme_fake", "1.0.0"), false)
	m := sv.manifest()
	if got := featureKeys(m); !slices.Equal(got, []string{"acme.backup", "acme.tools", "ops.audit"}) {
		t.Fatalf("features after a runtime install = %v", got)
	}
	// Nothing was pushed for the package's keys yet; the module's key is
	// still answered by the module.
	if f := feature(m, "acme.backup"); f.Access != "locked" || feature(m, "ops.audit").Access != "full" {
		t.Fatalf("acme.backup %+v, ops.audit %+v", f, feature(m, "ops.audit"))
	}

	// A compiled module that provides features and a package that
	// declares some boot together.
	sv.stop()
	sv = s.boot()
	if got := featureKeys(sv.manifest()); !slices.Equal(got, []string{"acme.backup", "acme.tools", "ops.audit"}) {
		t.Fatalf("features after a restart = %v", got)
	}
	sv.uninstall("acme_fake", false)
	if got := featureKeys(sv.manifest()); !slices.Equal(got, []string{"ops.audit"}) {
		t.Fatalf("features after uninstall = %v", got)
	}
}

// A package declaring a key a compiled module provides is refused at
// install, before approval, naming the module.
func TestContractInstallRefusesACompiledModulesFeatureKey(t *testing.T) {
	s := newSite(t)
	s.modules = []app.Module{opsModule{key: "acme.tools"}}
	sv := s.boot()
	status, body := sv.do(http.MethodPost, "/api/v1/plugins/install", bytes.NewReader(s.fakePackage("acme_fake", "1.0.0")))
	var refusal struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
	if status != http.StatusConflict || json.Unmarshal(body, &refusal) != nil || refusal.Code != messages.ErrorPluginFeatureTaken ||
		refusal.Params["id"] != "acme_fake" || refusal.Params["feature"] != "acme.tools" || refusal.Params["owner"] != "ops" {
		t.Fatalf("install = %d %s", status, body)
	}
	if p := sv.plugin("acme_fake"); p.State != "absent" {
		t.Fatalf("a refused package is listed: %+v", p)
	}
	if got := featureKeys(sv.manifest()); !slices.Equal(got, []string{"acme.tools"}) {
		t.Fatalf("features = %v", got)
	}
}
