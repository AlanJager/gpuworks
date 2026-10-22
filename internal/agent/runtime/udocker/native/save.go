package native

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SaveArchive writes a registered image to w as an image archive in the
// layout Docker's containerd image store produces: an OCI image layout
// (oci-layout, index.json, blobs/sha256/...) plus a docker-save manifest.json
// whose paths point into blobs/. Docker (either image store), nerdctl and
// LoadArchive all load it.
//
// Blobs are streamed as stored -- the registry's compressed layers and the
// manifest bytes verbatim -- so the archive costs no recompression and the
// image keeps its identity: the manifest digest, which is the image ID, is the
// same on the receiving node.
func (l Layout) SaveArchive(ctx context.Context, ref Ref, w io.Writer) error {
	if !l.HasImage(ref) {
		return fmt.Errorf("image %s:%s is not in the native image store", ref.Local, ref.Tag)
	}
	manifestJSON, err := os.ReadFile(filepath.Join(l.TagDir(ref), "manifest"))
	if err != nil {
		return err
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		return fmt.Errorf("decode stored manifest: %w", err)
	}
	mediaType := manifest.MediaType
	if mediaType == "" {
		mediaType = mediaOCIManifest
	}
	manifestDigest := digestOf(manifestJSON)
	blobPath := func(digest string) string { return "blobs/sha256/" + strings.TrimPrefix(digest, "sha256:") }

	// Docker's containerd store takes io.containerd.image.name verbatim as the
	// image name and only finds fully qualified ones; manifest.json keeps the
	// familiar name, as docker save writes it. A digest reference names no tag.
	annotations := map[string]string{"io.containerd.image.name": ref.FullName()}
	repoTags := []string{}
	if !ref.IsDigest() {
		annotations["org.opencontainers.image.ref.name"] = ref.Tag
		repoTags = append(repoTags, ref.Local+":"+ref.Tag)
	}
	index, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{{
			"mediaType":   mediaType,
			"digest":      manifestDigest,
			"size":        len(manifestJSON),
			"annotations": annotations,
		}},
	})
	if err != nil {
		return err
	}
	layers := make([]string, 0, len(manifest.Layers))
	for _, layer := range manifest.Layers {
		layers = append(layers, blobPath(layer.Digest))
	}
	dockerManifest, err := json.Marshal([]map[string]any{{
		"Config":   blobPath(manifest.Config.Digest),
		"RepoTags": repoTags,
		"Layers":   layers,
	}})
	if err != nil {
		return err
	}

	tw := tar.NewWriter(w)
	epoch := time.Unix(0, 0)
	writeBytes := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data)), ModTime: epoch}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: "blobs/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: epoch}); err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: "blobs/sha256/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: epoch}); err != nil {
		return err
	}
	written := map[string]bool{}
	for _, desc := range append([]Descriptor{manifest.Config}, manifest.Layers...) {
		if written[desc.Digest] {
			continue // an image may repeat a layer (e.g. empty ones)
		}
		written[desc.Digest] = true
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := l.writeBlob(tw, blobPath(desc.Digest), desc, epoch); err != nil {
			return err
		}
	}
	if !written[manifestDigest] {
		if err := writeBytes(blobPath(manifestDigest), manifestJSON); err != nil {
			return err
		}
	}
	if err := writeBytes("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)); err != nil {
		return err
	}
	if err := writeBytes("index.json", index); err != nil {
		return err
	}
	if err := writeBytes("manifest.json", dockerManifest); err != nil {
		return err
	}
	return tw.Close()
}

// writeBlob streams one stored blob into the archive, checking it still has
// the size its manifest records.
func (l Layout) writeBlob(tw *tar.Writer, name string, desc Descriptor, modTime time.Time) error {
	file, err := os.Open(filepath.Join(l.LayersDir(), desc.Digest))
	if err != nil {
		return fmt.Errorf("open blob %s: %w", shortDigest(desc.Digest), err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != desc.Size {
		return fmt.Errorf("blob %s is %d bytes, manifest says %d", shortDigest(desc.Digest), info.Size(), desc.Size)
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: desc.Size, ModTime: modTime}); err != nil {
		return err
	}
	if _, err := io.Copy(tw, file); err != nil {
		return fmt.Errorf("write blob %s: %w", shortDigest(desc.Digest), err)
	}
	return nil
}
