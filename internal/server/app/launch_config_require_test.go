package app

import (
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestRequireNormalizedLaunchConfigsDoesNotWriteLegacyRows(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "launch.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := requireNormalizedLaunchConfigs(st); err != nil {
		t.Fatalf("store without deployments failed the read-only check: %v", err)
	}
	created, err := st.CreateDeployment(legacyDeployment())
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.GetDeployment(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := requireNormalizedLaunchConfigs(st); err == nil || !strings.Contains(err.Error(), "requires launch configuration normalization") {
		t.Fatalf("legacy row passed read-only check: %v", err)
	}
	after, err := st.GetDeployment(created.ID)
	if err != nil || after.QuickParams != before.QuickParams || after.ExtraArgs != before.ExtraArgs || after.CatalogSelectionSnapshot != before.CatalogSelectionSnapshot || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("read-only check changed deployment: before=%+v after=%+v err=%v", before, after, err)
	}
	if err := normalizeLaunchConfigs(st); err != nil {
		t.Fatal(err)
	}
	if err := requireNormalizedLaunchConfigs(st); err != nil {
		t.Fatalf("normalized row failed read-only check: %v", err)
	}
	// An unreadable stored configuration fails the check as it fails the
	// rewrite: neither may pass over a row nobody can account for.
	if err := st.UpdateDeploymentLaunchConfig(created.ID, "{not json", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := requireNormalizedLaunchConfigs(st); err == nil || strings.Contains(err.Error(), "requires launch configuration normalization") {
		t.Fatalf("unreadable configuration: %v", err)
	}
}
