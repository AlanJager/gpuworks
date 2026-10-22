package seed

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// systemImageRepository is the repository the embedded image catalog carries:
// the gpuworks-runner matrix (imagecatalog's TestBuiltinSeedIsRunnerMatrix).
// Other gpuworks images a template may run, such as gpuworks/strata, are not
// catalog images: their templates declare the copies themselves
// (TestBuiltinTemplateOtherGpuworksImagesDeclareSources).
const systemImageRepository = "gpuworks/gpuworks-runner:"

// A built-in template that runs a runner image gets its pull sources from the
// image catalog row of that image, not from image_sources of its own (ADR 0038:
// the platform's mirrors belong to system images). That only holds while every
// such image still resolves to a row of the embedded image catalog: bumping the
// runner matrix without moving the templates would leave them with the
// canonical Docker Hub reference as their only source, and nothing else would
// notice. The check goes through the store, the way a deployment resolves it.
func TestBuiltinTemplateSystemImagesResolveToImageCatalogRows(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "system-images.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := imagecatalog.SyncSeed(st); err != nil {
		t.Fatal(err)
	}

	users := map[string][]string{}
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			if strings.HasPrefix(spec.Image, systemImageRepository) {
				users[spec.Image] = append(users[spec.Image], model.ModelID+"/"+spec.Name)
			}
		}
	}
	if len(users) == 0 {
		t.Fatal("no built-in template runs a runner image; the check would pass vacuously")
	}
	images := make([]string, 0, len(users))
	for image := range users {
		images = append(images, image)
	}
	sort.Strings(images)
	for _, image := range images {
		row, err := st.ResolveImageRef(image)
		if err != nil {
			t.Errorf("%s (used by %d specs, e.g. %s) is not in the embedded image catalog: %v", image, len(users[image]), users[image][0], err)
			continue
		}
		var sources []string
		if err := json.Unmarshal([]byte(row.RegistrySources), &sources); err != nil {
			t.Errorf("%s: catalog registry_sources do not decode: %v", image, err)
			continue
		}
		registry := 0
		for _, source := range sources {
			if !imagecatalog.IsArchiveURL(source) {
				registry++
			}
		}
		if registry == 0 {
			t.Errorf("%s resolves to catalog row %s without registry sources; templates using it could only pull the canonical reference", image, row.ImageRef)
		}
	}
}

// A gpuworks image outside the runner matrix has no image catalog row, so the
// platform offers no mirrors for it; gpuworks publishes its images to quay, SWR
// and registry.zstack.ai as well, and a built-in template running one lists
// those copies in image_sources, or a mainland deployment could only try
// Docker Hub.
func TestBuiltinTemplateOtherGpuworksImagesDeclareSources(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			if !strings.HasPrefix(spec.Image, "gpuworks/") || strings.HasPrefix(spec.Image, systemImageRepository) {
				continue
			}
			checked++
			if len(spec.ImageSources) == 0 {
				t.Errorf("%s/%s runs %s without image_sources", model.ModelID, spec.Name, spec.Image)
			}
		}
	}
	if checked == 0 {
		t.Log("no built-in template runs a gpuworks image outside the runner matrix")
	}
}
