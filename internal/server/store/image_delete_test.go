package store

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestDeleteImageCleansNodesAndLastSeries(t *testing.T) {
	s := newTestStore(t)
	series := &ImageSeries{Name: "custom-delete", Source: "custom", Enabled: true}
	if err := s.DB().Create(series).Error; err != nil {
		t.Fatal(err)
	}
	image := &Image{SeriesID: series.ID, ImageRef: "example/custom:v1", Source: "custom", Enabled: true}
	if err := s.DB().Create(image).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertImageNode(&ImageNode{ImageVersionID: image.ID, NodeID: 1, State: ImageStatePresent}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteImage(image.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetImage(image.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("image lookup = %v", err)
	}
	if _, err := s.GetImageSeries(series.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("series lookup = %v", err)
	}
	var nodes int64
	if err := s.DB().Model(&ImageNode{}).Where("image_version_id = ?", image.ID).Count(&nodes).Error; err != nil {
		t.Fatal(err)
	}
	if nodes != 0 {
		t.Fatalf("image nodes remaining = %d", nodes)
	}
}

func TestDeleteImageRejectsOfficialAndRunningReference(t *testing.T) {
	s := newTestStore(t)
	series := &ImageSeries{Name: "community-delete", Source: "community", Enabled: true}
	if err := s.DB().Create(series).Error; err != nil {
		t.Fatal(err)
	}
	image := &Image{SeriesID: series.ID, ImageRef: "example/community:v1", Source: "community", Enabled: true}
	if err := s.DB().Create(image).Error; err != nil {
		t.Fatal(err)
	}
	dep, err := s.CreateDeployment(&Deployment{State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInstance(&Instance{UUID: "image-reference", DeploymentID: dep.ID, Image: image.ImageRef, State: proto.InstRunning}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteImage(image.ID); !errors.Is(err, ErrReferenced) {
		t.Fatalf("DeleteImage error = %v", err)
	}
	officialSeries := &ImageSeries{Name: "official-delete", Source: "official", Enabled: true}
	if err := s.DB().Create(officialSeries).Error; err != nil {
		t.Fatal(err)
	}
	official := &Image{SeriesID: officialSeries.ID, ImageRef: "official/vllm:v1", Source: "official", Enabled: true}
	if err := s.DB().Create(official).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteImage(official.ID); err == nil {
		t.Fatal("official image deletion unexpectedly succeeded")
	}
}

// A custom image is protected by the same reference rule as a system one: a
// deployment that names it or a model bound to it keeps it, while an instance
// that already finished does not.
func TestDeleteImageUsesTheCatalogReferenceRule(t *testing.T) {
	s := newTestStore(t)
	add := func(ref string) *Image {
		t.Helper()
		image, err := s.UpsertImage(&Image{ImageRef: ref, Engine: "vllm", Framework: "cuda", Source: "custom", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		return image
	}
	byDeployment, byModel, byStopped := add("example/by-deployment:v1"), add("example/by-model:v1"), add("example/by-stopped:v1")
	if err := s.DB().Create(&Deployment{Name: "names-it", State: "stopped", Image: byDeployment.ImageRef}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB().Create(&Model{Name: "bound", ImageID: byModel.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB().Create(&Instance{UUID: "finished", State: proto.InstStopped, Image: byStopped.ImageRef}).Error; err != nil {
		t.Fatal(err)
	}
	for _, image := range []*Image{byDeployment, byModel} {
		if err := s.DeleteImage(image.ID); !errors.Is(err, ErrReferenced) {
			t.Fatalf("DeleteImage(%s) = %v, want ErrReferenced", image.ImageRef, err)
		}
		if err := s.DeleteImageSeries(image.SeriesID); !errors.Is(err, ErrReferenced) {
			t.Fatalf("DeleteImageSeries(%s) = %v, want ErrReferenced", image.ImageRef, err)
		}
	}
	if err := s.DeleteImage(byStopped.ID); err != nil {
		t.Fatalf("an image only a finished instance used must be deletable: %v", err)
	}
}

// Re-importing the catalog writes a system row's description, never the
// operator's switch on it.
func TestUpsertSystemRowKeepsEnabled(t *testing.T) {
	s := newTestStore(t)
	series, err := s.UpsertImageSeries(&ImageSeries{Name: "sys", Engine: "vllm", Framework: "cuda", Source: "official", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	image, err := s.UpsertImage(&Image{SeriesID: series.ID, ImageRef: "sys/runner:v1", Engine: "vllm", Source: "official", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetImageEnabled(image.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetImageSeriesEnabled(series.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertImageSeries(&ImageSeries{Name: "sys", Engine: "vllm", Framework: "cuda", Source: "official", Notes: "new", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	updated, err := s.UpsertImage(&Image{SeriesID: series.ID, ImageRef: "sys/runner:v1", Engine: "vllm", EngineVersion: "0.2", Source: "official", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	row, err := s.GetImageSeries(series.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Enabled || updated.EngineVersion != "0.2" || row.Enabled || row.Notes != "new" {
		t.Fatalf("version=%+v series=%+v", updated, row)
	}
	// A custom row is the operator's own: an upsert writes its switch.
	custom, err := s.UpsertImage(&Image{ImageRef: "own/runner:v1", Engine: "vllm", Source: "custom", Enabled: false})
	if err != nil || custom.Enabled {
		t.Fatalf("custom = %+v, %v", custom, err)
	}
	if custom, err = s.UpsertImage(&Image{SeriesID: custom.SeriesID, ImageRef: "own/runner:v1", Engine: "vllm", Source: "custom", Enabled: true}); err != nil || !custom.Enabled {
		t.Fatalf("custom after upsert = %+v, %v", custom, err)
	}
}
