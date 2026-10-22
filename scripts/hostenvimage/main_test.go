package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func testLayer(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	body := []byte("#!/bin/sh\nexec python3 -m vllm \"$@\"\n")
	if err := tw.WriteHeader(&tar.Header{Name: "bin/vllm", Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "layer.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Equal inputs give an equal archive, byte for byte: a package's digest
// depends only on what went into it. The archive is what `docker load` and
// the agent read: a manifest naming the tag, a config for darwin/arm64 with
// the environment, labels and working directory, and the layer whose
// uncompressed digest the config records.
func TestRunWritesADeterministicDockerSaveArchive(t *testing.T) {
	layer := testLayer(t)
	const tag = "gpuworks/gpuworks-runner:metal15.0-vllm0.31.0-r2"
	build := func() []byte {
		out := filepath.Join(t.TempDir(), "package.tar")
		if err := run(layer, tag, "darwin", "arm64", "/vllm-workspace", []string{"PATH=/bin:/python/bin"}, []string{"ai.gpuworks.host-env.engine=vllm-metal"}, out); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	first, second := build(), build()
	if !bytes.Equal(first, second) {
		t.Fatal("two builds of the same inputs differ")
	}

	members := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(first))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		members[h.Name], _ = io.ReadAll(tr)
	}
	var manifest []struct {
		Config   string
		RepoTags []string
		Layers   []string
	}
	if err := json.Unmarshal(members["manifest.json"], &manifest); err != nil || len(manifest) != 1 {
		t.Fatalf("manifest.json = %s, %v", members["manifest.json"], err)
	}
	m := manifest[0]
	if len(m.RepoTags) != 1 || m.RepoTags[0] != tag || len(m.Layers) != 1 || members[m.Layers[0]] == nil {
		t.Fatalf("manifest = %+v, members %v", m, len(members))
	}
	var config struct {
		Architecture, OS string
		Config           struct {
			Env        []string
			Labels     map[string]string
			WorkingDir string
		}
		RootFS struct {
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	if err := json.Unmarshal(members[m.Config], &config); err != nil {
		t.Fatal(err)
	}
	diffID, err := uncompressedDigest(members[m.Layers[0]])
	if err != nil {
		t.Fatal(err)
	}
	if config.Architecture != "arm64" || config.OS != "darwin" || config.Config.WorkingDir != "/vllm-workspace" ||
		len(config.Config.Env) != 1 || config.Config.Labels["ai.gpuworks.host-env.engine"] != "vllm-metal" ||
		len(config.RootFS.DiffIDs) != 1 || config.RootFS.DiffIDs[0] != "sha256:"+diffID {
		t.Fatalf("config = %+v", config)
	}
	if m.Config != digest(members[m.Config])+".json" {
		t.Fatal("the config is not named by its digest")
	}

	if err := run(layer, tag, "darwin", "arm64", "", nil, []string{"no-equals"}, filepath.Join(t.TempDir(), "x.tar")); err == nil {
		t.Fatal("a label without = was accepted")
	}
	if err := run(filepath.Join(t.TempDir(), "missing"), tag, "darwin", "arm64", "", nil, nil, filepath.Join(t.TempDir(), "x.tar")); err == nil {
		t.Fatal("a missing layer was accepted")
	}
}
