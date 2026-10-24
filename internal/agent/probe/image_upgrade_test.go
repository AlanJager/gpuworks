package probe

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"

	"gpuworks.ai/gpuworks/internal/assets"
	"gpuworks.ai/gpuworks/internal/proto"
)

type cachedProbeDriver struct {
	fakeDriver
	loaded string
}

func (d *cachedProbeDriver) ImageExists(_ context.Context, image string) (bool, error) {
	return image == "gpuworks/partition-probe:v1" || image == d.loaded, nil
}

func (d *cachedProbeDriver) LoadImage(_ context.Context, image string, archive io.Reader) error {
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err != nil {
			return fmt.Errorf("probe manifest: %w", err)
		}
		if header.Name != "manifest.json" {
			continue
		}
		var manifest []struct{ RepoTags []string }
		if err := json.NewDecoder(tr).Decode(&manifest); err != nil {
			return err
		}
		if len(manifest) != 1 || len(manifest[0].RepoTags) != 1 || manifest[0].RepoTags[0] != image {
			return fmt.Errorf("archive tags do not match requested image %s: %+v", image, manifest)
		}
		d.loaded = image
		return nil
	}
}

func TestRunnerReplacesCachedProbeProtocol(t *testing.T) {
	driver := &cachedProbeDriver{}
	runner := New(driver, &fakeBuilder{}, "ascend")
	reports, gpus := ascendFacts()
	results := runner.Results(context.Background(), reports, gpus)
	if len(results) != 1 || results[0].Status != proto.PartitionProbePassed || driver.loaded != assets.PartitionProbeImage || driver.loaded == "gpuworks/partition-probe:v1" {
		t.Fatalf("old cached image bypassed the upgrade: loaded=%q results=%+v", driver.loaded, results)
	}
	if len(driver.runs) != 1 || driver.runs[0].Image != driver.loaded {
		t.Fatalf("probe did not run the loaded image: %+v", driver.runs)
	}
}
