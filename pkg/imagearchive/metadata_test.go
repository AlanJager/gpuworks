package imagearchive

import (
	"archive/tar"
	"bytes"
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
