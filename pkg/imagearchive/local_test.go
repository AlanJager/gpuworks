package imagearchive

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
)

func TestReadRepoTagsPrefersTopLevelMetadata(t *testing.T) {
	refs := []string{"registry:5000/team/runner:v2", "example/runner:latest"}
	docker := dockerSaveArchive(t, dockerSaveOptions{repoTags: refs, layerSize: 2 << 20})
	for _, tc := range []struct {
		name string
		data []byte
		want []string
	}{
		{"docker", docker, refs},
		{"gzip", gzipBytes(t, docker), refs},
		{"oci", ociArchive(t, refs, 4096), refs},
		{"untagged", ociArchive(t, []string{"latest"}, 1024), nil},
		{"layer contains a decoy manifest", buildTar(t, tar.FormatUSTAR, []tarEntry{{name: "layer.tar", data: docker}}), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReadRepoTags(t.Context(), bytes.NewReader(tc.data))
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, %v, want %v", got, err, tc.want)
			}
		})
	}
}

func TestReadRepoTagsRejectsInvalidOrOversizedMetadataAndStopsOnCancel(t *testing.T) {
	large := buildTar(t, tar.FormatUSTAR, []tarEntry{{name: "manifest.json", data: make([]byte, tailWindowLarge+1)}})
	for _, data := range [][]byte{[]byte("not a tar"), large} {
		if _, err := ReadRepoTags(t.Context(), bytes.NewReader(data)); err == nil {
			t.Fatal("invalid archive accepted")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ReadRepoTags(ctx, bytes.NewReader(large)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
}
