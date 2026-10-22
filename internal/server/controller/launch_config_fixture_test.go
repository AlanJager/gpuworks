package controller

import (
	"encoding/json"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// createTestDeployment stores a fixture deployment in the shape the server
// guarantees a deployment row has: one complete launch configuration, the user
// arguments the parameter dictionary does not describe, and a catalog snapshot
// holding only the template's own residual arguments.
//
// Fixtures are written the way a person describes a case - a couple of
// interesting parameters - and this closes the gap by running the same
// normalization the server applies at startup. A controller test that launched
// from a row no write path can produce would be measuring a shape that does not
// exist.
// A single-node, non-PD deployment is pinned to the node it names, as every
// such row is after migration 120 and as a manual create stores it; a test of
// the candidate pool states its requirements through
// createTestDeploymentWithRequirements instead.
func createTestDeployment(t *testing.T, st *store.Store, dep *store.Deployment) (*store.Deployment, error) {
	t.Helper()
	var reqs map[string][]string
	if dep.NNodes <= 1 && dep.NodeID != 0 && !strings.Contains(dep.Placement, `"pd"`) {
		if node, err := st.NodeByID(dep.NodeID); err == nil {
			reqs = map[string][]string{"gpuworks.ai/node": {node.UUID}}
		}
	}
	return st.CreateDeploymentWithRequirements(normalizeFixtureLaunchConfig(t, dep), reqs)
}

// createTestDeploymentWithRequirements is createTestDeployment for a
// deployment that bounds where it may run.
func createTestDeploymentWithRequirements(t *testing.T, st *store.Store, dep *store.Deployment, reqs map[string][]string) (*store.Deployment, error) {
	t.Helper()
	return st.CreateDeploymentWithRequirements(normalizeFixtureLaunchConfig(t, dep), reqs)
}

func normalizeFixtureLaunchConfig(t *testing.T, dep *store.Deployment) *store.Deployment {
	t.Helper()
	// Every write path stores the image it resolved at create time, so a row
	// without one is a shape no production write can produce; fixtures that do
	// not care get the engine's default image, the resolver's own last answer.
	if strings.TrimSpace(dep.Image) == "" {
		if profile, err := deploy.ProfileForEngine(strings.TrimSpace(dep.Engine)); err == nil && profile.DefaultImage != "" {
			dep.Image = profile.DefaultImage
		}
	}
	snapshot := map[string]any{}
	if strings.TrimSpace(dep.CatalogSelectionSnapshot) != "" {
		if err := json.Unmarshal([]byte(dep.CatalogSelectionSnapshot), &snapshot); err != nil {
			t.Fatalf("fixture catalog snapshot: %v", err)
		}
	}
	var templateArgs []string
	if raw, ok := snapshot["extra_args"]; ok {
		encoded, err := json.Marshal(raw)
		if err != nil {
			t.Fatalf("fixture snapshot extra_args: %v", err)
		}
		if err := json.Unmarshal(encoded, &templateArgs); err != nil {
			t.Fatalf("fixture snapshot extra_args: %v", err)
		}
	}
	out, _, err := deploy.NormalizeStoredLaunchConfig(deploy.StoredLaunchConfig{
		Engine: dep.Engine, QuickParamsJSON: dep.QuickParams, ExtraArgsText: dep.ExtraArgs, TemplateArgs: templateArgs,
	})
	if err != nil {
		t.Fatalf("fixture launch configuration: %v", err)
	}
	dep.QuickParams, dep.ExtraArgs = out.QuickParamsJSON, out.ExtraArgsText
	if len(snapshot) > 0 {
		snapshot["extra_args"] = out.TemplateArgs
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatalf("fixture catalog snapshot: %v", err)
		}
		dep.CatalogSelectionSnapshot = string(encoded)
	}
	return dep
}
