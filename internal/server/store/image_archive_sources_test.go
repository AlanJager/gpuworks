package store

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

const clusterArchiveURL = "https://containerimages.example/gpustack_runner--gpu3.tar"

func officialImageForArchiveSources(t *testing.T, dsn string) (*Store, *Image) {
	t.Helper()
	st, err := Open(dsn, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	series, err := st.UpsertImageSeries(&ImageSeries{Name: "vllm-cann", Engine: "vllm", Source: "official", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	image, err := st.UpsertImage(&Image{
		SeriesID: series.ID, Name: "cann", Engine: "vllm", Enabled: true,
		ImageRef: "gpustack/runner:cann8.5-910b-vllm0.18.0", Source: "official",
		EngineVersion: "0.18.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	return st, image
}

func storedSources(t *testing.T, st *Store, id uint) []string {
	t.Helper()
	row, err := st.GetImage(id)
	if err != nil {
		t.Fatal(err)
	}
	var sources []string
	if err := json.Unmarshal([]byte(row.RegistrySources), &sources); err != nil {
		t.Fatal(err)
	}
	return sources
}

// An official version is catalog-owned except for the addresses this cluster
// can reach it on. The catalog cannot know that an operator has the runner as
// an HTTP archive, and without this the image cannot be fetched on any node.
func TestOfficialImageAcceptsAClusterArchiveAddress(t *testing.T) {
	st, official := officialImageForArchiveSources(t, "file:official_archive_address?mode=memory&cache=shared")
	encoded, err := json.Marshal([]string{clusterArchiveURL})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := st.UpdateImageRegistrySources(official.ID, string(encoded))
	if err != nil {
		t.Fatalf("an official image must accept an acquisition address: %v", err)
	}
	if updated.EngineVersion != official.EngineVersion || updated.Source != "official" {
		t.Fatalf("the catalog's own fields moved: %+v", updated)
	}
	if sources := storedSources(t, st, official.ID); len(sources) != 1 || sources[0] != clusterArchiveURL {
		t.Fatalf("registry_sources = %v", sources)
	}
}

// A catalog import rewrites the row, so the address an operator added has to
// survive it — otherwise the next manifest sync silently breaks every node
// that fetches this image from the archive.
func TestCatalogReimportKeepsAClusterArchiveAddress(t *testing.T) {
	st, official := officialImageForArchiveSources(t, "file:catalog_reimport_archive_address?mode=memory&cache=shared")
	encoded, err := json.Marshal([]string{"quay.io/gpustack/runner:cann8.5-910b-vllm0.18.0", clusterArchiveURL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateImageRegistrySources(official.ID, string(encoded)); err != nil {
		t.Fatal(err)
	}
	catalogEntry, err := json.Marshal([]string{"quay.io/gpustack/runner:cann8.5-910b-vllm0.18.1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImage(&Image{
		SeriesID: official.SeriesID, Name: "cann", Engine: "vllm", Enabled: true,
		ImageRef: official.ImageRef, Source: "official", RegistrySources: string(catalogEntry),
	}); err != nil {
		t.Fatal(err)
	}
	// The catalog's reference replaces the one it dropped, and the address the
	// cluster added is still there.
	want := []string{"quay.io/gpustack/runner:cann8.5-910b-vllm0.18.1", clusterArchiveURL}
	if sources := storedSources(t, st, official.ID); len(sources) != len(want) || sources[0] != want[0] || sources[1] != want[1] {
		t.Fatalf("registry_sources = %v, want %v", sources, want)
	}
}
