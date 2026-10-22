package imagearchive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// The builders below reproduce the on-disk layouts this package must parse:
// `docker save` (config blob, layer directories, manifest.json, legacy
// repositories, two zero blocks) and an OCI layout (oci-layout, index.json,
// content-addressed blobs).

var fixedModTime = time.Unix(1_700_000_000, 0).UTC()

type tarEntry struct {
	name string
	dir  bool
	data []byte
}

func buildTar(t *testing.T, format tar.Format, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     0o644,
			Size:     int64(len(e.data)),
			ModTime:  fixedModTime,
			Format:   format,
			Typeflag: tar.TypeReg,
		}
		if e.dir {
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", e.name, err)
		}
		if !e.dir {
			if _, err := tw.Write(e.data); err != nil {
				t.Fatalf("write body %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// layerTar builds a layer payload, which is itself a tar. With rootDecoy it
// carries a root-level manifest.json, so a scanner that walks into layer
// payloads would report the decoy tag; otherwise the only manifest.json sits
// on a nested path, as in a real image layer.
func layerTar(t *testing.T, sizeHint int, rootDecoy bool) []byte {
	t.Helper()
	decoy, err := json.Marshal([]map[string]any{{"Config": "decoy.json", "RepoTags": []string{"decoy/inner:latest"}}})
	if err != nil {
		t.Fatalf("marshal decoy: %v", err)
	}
	name := "etc/manifest.json"
	if rootDecoy {
		name = "manifest.json"
	}
	filler := bytes.Repeat([]byte("payload\n"), sizeHint/8+1)
	return buildTar(t, tar.FormatUSTAR, []tarEntry{
		{name: name, data: decoy},
		{name: "usr/share/blob.bin", data: filler},
	})
}

type dockerSaveOptions struct {
	repoTags  []string
	layerSize int
	format    tar.Format
	// omitManifest drops manifest.json and the legacy repositories entry,
	// producing an archive whose tags cannot be determined.
	omitManifest bool
	// plainLayer stores opaque bytes instead of a nested tar carrying a decoy
	// manifest.json.
	plainLayer bool
}

func dockerSaveArchive(t *testing.T, opts dockerSaveOptions) []byte {
	t.Helper()
	if opts.format == tar.FormatUnknown {
		opts.format = tar.FormatUSTAR
	}
	if opts.layerSize == 0 {
		opts.layerSize = 4096
	}
	config := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":["sha256:aa"]}}`)
	configName := digestOf(config) + ".json"

	var layer []byte
	if opts.omitManifest || opts.plainLayer {
		layer = bytes.Repeat([]byte("layerbytes"), opts.layerSize/10+1)
	} else {
		layer = layerTar(t, opts.layerSize, true)
	}
	layerDir := digestOf(layer)

	entries := []tarEntry{
		{name: configName, data: config},
		{name: layerDir + "/", dir: true},
		{name: layerDir + "/VERSION", data: []byte("1.0")},
		{name: layerDir + "/json", data: []byte(`{"id":"` + layerDir + `"}`)},
		{name: layerDir + "/layer.tar", data: layer},
	}
	if !opts.omitManifest {
		manifest, err := json.Marshal([]map[string]any{{
			"Config":   configName,
			"RepoTags": opts.repoTags,
			"Layers":   []string{layerDir + "/layer.tar"},
		}})
		if err != nil {
			t.Fatalf("marshal manifest: %v", err)
		}
		repositories := map[string]map[string]string{}
		// docker's legacy repositories entry stays small; keeping every tag
		// here would dwarf manifest.json and change what the tail window holds.
		legacy := opts.repoTags
		if len(legacy) > 3 {
			legacy = legacy[:3]
		}
		for _, tag := range legacy {
			name, ref, ok := strings.Cut(tag, ":")
			if !ok {
				continue
			}
			if repositories[name] == nil {
				repositories[name] = map[string]string{}
			}
			repositories[name][ref] = layerDir
		}
		repoJSON, err := json.Marshal(repositories)
		if err != nil {
			t.Fatalf("marshal repositories: %v", err)
		}
		// docker writes manifest.json first, then the legacy repositories file.
		entries = append(entries,
			tarEntry{name: "manifest.json", data: manifest},
			tarEntry{name: "repositories", data: repoJSON},
		)
	}
	return buildTar(t, opts.format, entries)
}

func ociArchive(t *testing.T, refNames []string, layerSize int) []byte {
	t.Helper()
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	layer := layerTar(t, layerSize, false)
	imgManifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    "sha256:" + digestOf(config),
			"size":      len(config),
		},
		"layers": []map[string]any{{
			"mediaType": "application/vnd.oci.image.layer.v1.tar",
			"digest":    "sha256:" + digestOf(layer),
			"size":      len(layer),
		}},
	})
	if err != nil {
		t.Fatalf("marshal image manifest: %v", err)
	}
	descriptors := make([]map[string]any, 0, len(refNames))
	for _, ref := range refNames {
		descriptors = append(descriptors, map[string]any{
			"mediaType":   "application/vnd.oci.image.manifest.v1+json",
			"digest":      "sha256:" + digestOf(imgManifest),
			"size":        len(imgManifest),
			"annotations": map[string]string{refNameAnnotation: ref},
		})
	}
	index, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests":     descriptors,
	})
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	return buildTar(t, tar.FormatGNU, []tarEntry{
		{name: "oci-layout", data: []byte(`{"imageLayoutVersion":"1.0.0"}`)},
		{name: "index.json", data: index},
		{name: "blobs/", dir: true},
		{name: "blobs/sha256/", dir: true},
		{name: "blobs/sha256/" + digestOf(config), data: config},
		{name: "blobs/sha256/" + digestOf(imgManifest), data: imgManifest},
		{name: "blobs/sha256/" + digestOf(layer), data: layer},
	})
}

// manyTags produces enough references that manifest.json alone exceeds 64 KiB.
func manyTags(n int) []string {
	tags := make([]string, 0, n)
	for i := range n {
		tags = append(tags, fmt.Sprintf("registry.example.com/gpustack/runner:cuda13.0-vllm0.25.1-build%06d", i))
	}
	return tags
}

// gzipBytes compresses data; the payload is incompressible so the result stays
// larger than the 64 KiB tail window.
func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func incompressible(n int) []byte {
	r := rand.New(rand.NewSource(7))
	b := make([]byte, n)
	_, _ = r.Read(b)
	return b
}
