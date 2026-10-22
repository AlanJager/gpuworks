package app

import (
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// legacyDeployment is a row in the shape the server wrote before the launch
// configuration had a single representation: a partial quick_params, a free-form
// extra_args that overrode it, and a catalog snapshot whose args carried more
// dictionary flags still.
func legacyDeployment() *store.Deployment {
	snapshot, err := json.Marshal(map[string]any{
		"template_id": 1,
		"spec_name":   "fp8-balanced",
		"extra_args":  []string{"--dtype", "bfloat16", "--load-format", "fastsafetensors"},
	})
	if err != nil {
		panic(err)
	}
	return &store.Deployment{
		Name: "legacy", ServedName: "legacy", Engine: "vllm", State: store.DeployActive,
		QuickParams:              `{"max_model_len":4096}`,
		ExtraArgs:                "--max-model-len 8192 --chat-template /model/chat.jinja",
		CatalogSelectionSnapshot: string(snapshot),
	}
}

func openStoreWithLegacyRow(t *testing.T, dbPath string) (*store.Store, uint) {
	t.Helper()
	st, err := store.Open(dbPath, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(legacyDeployment())
	if err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	return st, dep.ID
}

func newTestServer(t *testing.T, dir string) (*App, error) {
	t.Helper()
	server, err := New(Config{
		Addr: "127.0.0.1:0", DataDir: dir,
		DBPath: filepath.Join(dir, "gpuworks.db"), AdminUser: "admin", AdminPassword: "password",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if server != nil {
		// New starts the proxy's route-table goroutine outside of Run; every
		// caller of this helper exercises startup directly without Run, so
		// close it here or it leaks for the rest of the test binary (ticking
		// against a since-closed db on every later test and -count
		// iteration - see the same note in adminbootstrap_test.go).
		t.Cleanup(func() { server.proxy.Close() })
	}
	return server, err
}

func TestStartupRewritesLegacyLaunchConfigsAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "gpuworks.db")
	st, id := openStoreWithLegacyRow(t, dbPath)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	server, err := newTestServer(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	dep, err := server.Store().GetDeployment(id)
	if err != nil {
		t.Fatal(err)
	}
	var quick map[string]any
	if err := json.Unmarshal([]byte(dep.QuickParams), &quick); err != nil {
		t.Fatal(err)
	}
	// Legacy precedence: extra_args beat the field, so 8192 survives, and the
	// template's dtype is split into the configuration. Nothing else appears:
	// normalization restructures what the row already said and never adds a
	// value, or an upgrade would change how a running deployment starts.
	if got := quick["max_model_len"]; got != float64(8192) {
		t.Fatalf("max_model_len=%v, want the extra_args value 8192", got)
	}
	if quick["dtype"] != "bfloat16" {
		t.Fatalf("dtype=%v, want the template's bfloat16", quick["dtype"])
	}
	if _, filled := quick["gpu_memory_utilization"]; filled {
		t.Fatalf("normalization invented a utilization: %v", quick)
	}
	if len(quick) != 2 {
		t.Fatalf("normalized configuration=%v, want only the two keys the row stated", quick)
	}
	if dep.ExtraArgs != "--chat-template /model/chat.jinja" {
		t.Fatalf("extra_args=%q, want only the residual", dep.ExtraArgs)
	}
	var snapshot struct {
		SpecName  string   `json:"spec_name"`
		ExtraArgs []string `json:"extra_args"`
	}
	if err := json.Unmarshal([]byte(dep.CatalogSelectionSnapshot), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ExtraArgs) != 2 || snapshot.ExtraArgs[0] != "--load-format" || snapshot.ExtraArgs[1] != "fastsafetensors" {
		t.Fatalf("snapshot extra_args=%v, want only the template residual", snapshot.ExtraArgs)
	}
	if snapshot.SpecName != "fp8-balanced" {
		t.Fatalf("the rest of the snapshot must survive: %+v", snapshot)
	}
	if err := server.Store().Close(); err != nil {
		t.Fatal(err)
	}

	// A second startup must change nothing at all.
	second, err := newTestServer(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Store().Close() })
	again, err := second.Store().GetDeployment(id)
	if err != nil {
		t.Fatal(err)
	}
	if again.QuickParams != dep.QuickParams || again.ExtraArgs != dep.ExtraArgs || again.CatalogSelectionSnapshot != dep.CatalogSelectionSnapshot {
		t.Fatalf("second startup rewrote the row:\nfirst=%+v\nsecond=%+v", dep, again)
	}
	if !again.UpdatedAt.Equal(dep.UpdatedAt) {
		t.Fatalf("second startup wrote the row again: updated_at %v then %v", dep.UpdatedAt, again.UpdatedAt)
	}
}

func TestStartupFailsAndLeavesRowsIntactWhenTheWriteFails(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "gpuworks.db")
	st, id := openStoreWithLegacyRow(t, dbPath)
	// Reject every update to the deployments table, which is the failure a
	// locked or constrained database presents to this pass.
	if err := st.DB().Exec(`CREATE TRIGGER reject_deployment_update BEFORE UPDATE ON deployments
BEGIN SELECT RAISE(ABORT, 'injected write failure'); END;`).Error; err != nil {
		t.Fatal(err)
	}
	before, err := st.GetDeployment(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	server, err := newTestServer(t, dir)
	if err == nil {
		_ = server.Store().Close()
		t.Fatal("a launch configuration that could not be written must abort startup")
	}

	reopened, err := store.Open(dbPath, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	after, err := reopened.GetDeployment(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.QuickParams != before.QuickParams || after.ExtraArgs != before.ExtraArgs || after.CatalogSelectionSnapshot != before.CatalogSelectionSnapshot {
		t.Fatalf("a failed normalization left the row modified:\nbefore=%+v\nafter=%+v", before, after)
	}
}

func TestNormalizeLaunchConfigsRejectsAnUnreadableStoredConfiguration(t *testing.T) {
	st, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.CreateDeployment(&store.Deployment{
		Name: "broken", ServedName: "broken", Engine: "vllm", State: store.DeployActive,
		QuickParams: `{"max_model_len":`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := normalizeLaunchConfigs(st); err == nil {
		t.Fatal("a configuration that cannot be read must not be silently replaced")
	}
}
