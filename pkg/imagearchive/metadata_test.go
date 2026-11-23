package imagearchive

import (
	"archive/tar"
	"bytes"
	"fmt"
	"reflect"
	"slices"
	"testing"
)

func TestArchiveMetadataFollowsTheConfigOfEachReference(t *testing.T) {
	first := []byte(`{"architecture":"amd64","config":{"Labels":{"io.gpuworks.engine":"vllm","io.gpuworks.engine.version":"0.30.0","io.gpuworks.framework":"cuda","io.gpuworks.framework.version":"13.0"}}}`)
	second := []byte(`{"architecture":"arm64"}`)
	manifest := []byte(`[{"Config":"sha256:first","RepoTags":["example/runner:one","example/runner:alias"]},{"Config":"second.json","RepoTags":["example/runner:two"]},{"Config":"missing.json","RepoTags":["example/runner:missing"]}]`)
	archive := buildTar(t, tar.FormatGNU, []tarEntry{
		{name: "sha256:first", data: first},
		{name: "big-layer.tar", data: make([]byte, 2<<20)},
		{name: "manifest.json", data: manifest},
		{name: "second.json", data: second},
	})
	full := ImageMetadata{Engine: "vllm", EngineVersion: "0.30.0", Framework: "cuda", FrameworkVersion: "13.0", Arch: "amd64"}
	want := map[string]ImageMetadata{"example/runner:one": full, "example/runner:alias": full, "example/runner:two": {Arch: "arm64"}}
	for _, data := range [][]byte{archive, gzipBytes(t, archive)} {
		got, err := ReadContents(t.Context(), bytes.NewReader(data))
		if err != nil || !reflect.DeepEqual(got.MetadataByTag, want) || !slices.Contains(got.RepoTags, "example/runner:missing") {
			t.Fatalf("local = %+v, %v", got, err)
		}
	}
	server, srv := newRangeServer(t, archive)
	got, err := InspectURL(t.Context(), srv.Client(), srv.URL+"/image.tar")
	if err != nil || !reflect.DeepEqual(got.MetadataByTag, want) || !slices.Contains(got.RepoTags, "example/runner:missing") {
		t.Fatalf("URL = %+v, %v", got, err)
	}
	if server.bytesServed() > maxFetchBytes {
		t.Errorf("URL inspection read %d bytes", server.bytesServed())
	}
}

func TestOCIArchiveMetadataFollowsIndexAndManifest(t *testing.T) {
	ref := "example/oci:v1"
	data := ociArchive(t, []string{ref}, 2<<20)
	local, err := ReadContents(t.Context(), bytes.NewReader(data))
	if err != nil || local.MetadataByTag[ref] != (ImageMetadata{Arch: "amd64"}) {
		t.Fatalf("local = %+v, %v", local, err)
	}
	_, srv := newRangeServer(t, data)
	remote, err := InspectURL(t.Context(), srv.Client(), srv.URL+"/image.tar")
	if err != nil || !reflect.DeepEqual(local.MetadataByTag, remote.MetadataByTag) {
		t.Fatalf("remote = %+v, %v", remote, err)
	}
}

func TestLocalPreviewDistinguishesMissingManifestFromUntaggedImage(t *testing.T) {
	for _, tc := range []struct {
		entry   tarEntry
		wantErr bool
	}{{tarEntry{name: "file.txt", data: []byte("text")}, true}, {tarEntry{name: "manifest.json", data: []byte(`[{"Config":"config.json","RepoTags":[]}]`)}, false}} {
		got, err := ReadContents(t.Context(), bytes.NewReader(buildTar(t, tar.FormatGNU, []tarEntry{tc.entry})))
		if (err != nil) != tc.wantErr || len(got.RepoTags) != 0 {
			t.Fatalf("%s: got %+v, err=%v", tc.entry.name, got, err)
		}
	}
}

func TestLocalArchiveRevisitsConfigAfterCandidateBudget(t *testing.T) {
	const ref = "example/late:v1"
	const config = "blobs/sha256/late-config"
	var layers []tarEntry
	for i := 0; i < 8; i++ {
		layers = append(layers, tarEntry{name: fmt.Sprintf("blobs/sha256/layer-%d", i), data: make([]byte, 1<<20)})
	}
	cfg := tarEntry{name: config, data: []byte(`{"architecture":"arm64","config":{"Labels":{"io.gpuworks.engine":"sglang"}}}`)}
	for name, entries := range map[string][]tarEntry{
		"docker": {cfg, {name: "manifest.json", data: []byte(`[{"Config":"blobs/sha256/late-config","RepoTags":["example/late:v1"]}]`)}},
		"oci":    {cfg, {name: "blobs/sha256/manifest", data: []byte(`{"config":{"digest":"sha256:late-config"}}`)}, {name: "index.json", data: []byte(`{"manifests":[{"digest":"sha256:manifest","annotations":{"org.opencontainers.image.ref.name":"example/late:v1"}}]}`)}},
	} {
		t.Run(name, func(t *testing.T) {
			tarball := buildTar(t, tar.FormatGNU, append(layers, entries...))
			for _, input := range [][]byte{tarball, gzipBytes(t, tarball)} {
				got, err := ReadContents(t.Context(), bytes.NewReader(input))
				if err != nil || !slices.Equal(got.RepoTags, []string{ref}) || got.MetadataByTag[ref] != (ImageMetadata{Arch: "arm64", Engine: "sglang"}) {
					t.Fatalf("contents = %+v, error = %v", got, err)
				}
			}
		})
	}
}
