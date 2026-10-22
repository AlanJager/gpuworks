package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestUpsertImageUpdatesAnExistingRow(t *testing.T) {
	st, err := Open("file:image_update_gpu_labels?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const ref = "example.com/partition-runner:v1"
	if _, err := st.UpsertImage(&Image{
		ImageRef: ref, Source: "custom", Engine: "vllm", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImage(&Image{
		ImageRef: ref, Source: "custom", Engine: "vllm", Enabled: true,
		EngineVersion: "0.9.2", Notes: "new notes", RegistrySources: `["https://mirror.example/runner.tar"]`, DeprecatedSince: "2026-10-01",
	}); err != nil {
		t.Fatal(err)
	}
	assertImageUpdated(t, st, ref, "0.9.2", "new notes", `["https://mirror.example/runner.tar"]`, "2026-10-01")
}

// assertImageUpdated reads the stored row back: an upsert of an existing image
// writes every column its Select list names.
func assertImageUpdated(t *testing.T, st *Store, ref, engineVersion, notes, registrySources, deprecatedSince string) {
	t.Helper()
	var row Image
	if err := st.db.Where("image_ref = ?", ref).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.EngineVersion != engineVersion || row.Notes != notes || row.RegistrySources != registrySources || row.DeprecatedSince != deprecatedSince {
		t.Fatalf("stored image = %+v", row)
	}
}
