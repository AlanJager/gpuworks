package native

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	kgzip "github.com/klauspost/compress/gzip"
)

// Media types written for layers of an archive-loaded image. The unpacker
// detects compression from magic bytes, so these only describe the blob.
const (
	mediaOCIManifest = "application/vnd.oci.image.manifest.v1+json"
	mediaOCIConfig   = "application/vnd.oci.image.config.v1+json"
	mediaLayerTar    = "application/vnd.oci.image.layer.v1.tar"
	mediaLayerGzip   = "application/vnd.oci.image.layer.v1.tar+gzip"
	mediaLayerZstd   = "application/vnd.oci.image.layer.v1.tar+zstd"
)

// maxArchiveMetadata bounds the JSON documents held in memory while loading.
const maxArchiveMetadata = 16 << 20

// archiveBlob is one file of the archive, stored in the layer store.
type archiveBlob struct {
	digest string
	size   int64
	magic  []byte
	data   []byte // kept for small files, which may be JSON metadata
}

// Load imports a `docker save` or OCI image archive read from r and registers
// it under image. It is the native replacement for `udocker load`, which
// untars the whole archive to a temporary directory first, stores it in the
// legacy v1 layout that native create cannot read, and fails on the nested
// index that Docker's containerd image store writes.
//
// The archive is read once, in any member order: every file goes straight
// into the layer store under its content digest, link members (the repeated
// layers of Docker before 25) take their target's blob, then the manifest is
// resolved -- from index.json when it names this platform (keeping the
// registry manifest digest as the image ID), else from manifest.json, which
// covers the classic layout and the mixed one newer Docker writes. Files the
// resolved image does not use are removed again.
func (e *Engine) Load(ctx context.Context, image string, r io.Reader) error {
	_, err := e.Layout.LoadArchive(ctx, ParseRef(image), e.Client.Platform, r)
	return err
}

// plainArchive undoes the gzip a `docker save | gzip` archive may carry, so
// the tar reader sees the tar. Docker's own load takes either form, and the
// partition probe images the agent embeds are stored gzipped.
func plainArchive(r io.Reader) (io.Reader, func(), error) {
	buffered := bufio.NewReader(r)
	if magic, _ := buffered.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := kgzip.NewReader(buffered)
		if err != nil {
			return nil, nil, fmt.Errorf("read image archive: %w", err)
		}
		return zr, func() { _ = zr.Close() }, nil
	}
	return buffered, func() {}, nil
}

// LoadArchive is Load against a layout; it returns the registered image.
func (l Layout) LoadArchive(ctx context.Context, ref Ref, platform Platform, r io.Reader) (*Image, error) {
	if err := os.MkdirAll(l.LayersDir(), 0o755); err != nil {
		return nil, err
	}
	files := map[string]*archiveBlob{}
	links := archiveLinks{}
	created := map[string]bool{}
	success := false
	defer func() {
		if !success {
			for digest := range created {
				_ = os.Remove(filepath.Join(l.LayersDir(), digest))
			}
		}
	}()
	plain, closePlain, err := plainArchive(r)
	if err != nil {
		return nil, err
	}
	defer closePlain()
	reader := tar.NewReader(plain)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read image archive: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(header.Name, "./"))
		switch header.Typeflag {
		case tar.TypeReg:
			delete(links, name)
		case tar.TypeSymlink, tar.TypeLink:
			// Its target may come later in the stream; resolved below.
			delete(files, name)
			links.add(header, name)
			continue
		default:
			continue
		}
		blob, isNew, err := l.storeArchiveFile(reader)
		if err != nil {
			return nil, fmt.Errorf("read image archive member %s: %w", name, err)
		}
		if isNew {
			created[blob.digest] = true
		}
		if hexName, ok := strings.CutPrefix(name, "blobs/sha256/"); ok && "sha256:"+hexName != blob.digest {
			return nil, fmt.Errorf("archive blob %s is corrupt: content digest is %s", name, blob.digest)
		}
		files[name] = blob
	}

	broken, err := links.resolve(files)
	if err != nil {
		return nil, err
	}
	image, err := resolveArchiveImage(files, broken, ref, platform)
	if err != nil {
		return nil, err
	}
	if err := verifyDiffIDs(files, image); err != nil {
		return nil, err
	}
	if err := l.RegisterImage(image); err != nil {
		return nil, err
	}
	used := map[string]bool{image.Manifest.Config.Digest: true}
	for _, layer := range image.Manifest.Layers {
		used[layer.Digest] = true
	}
	for digest := range created {
		if !used[digest] {
			_ = os.Remove(filepath.Join(l.LayersDir(), digest))
		}
	}
	success = true
	return image, nil
}

// storeArchiveFile streams one archive member into the layer store under its
// sha256, reporting whether this call created the blob.
func (l Layout) storeArchiveFile(r io.Reader) (*archiveBlob, bool, error) {
	tmp, err := os.CreateTemp(l.LayersDir(), ".loading-*")
	if err != nil {
		return nil, false, err
	}
	defer os.Remove(tmp.Name())
	hash := sha256.New()
	head := &headBuffer{limit: maxArchiveMetadata}
	size, err := io.Copy(io.MultiWriter(tmp, hash, head), r)
	if err != nil {
		tmp.Close()
		return nil, false, err
	}
	if err := tmp.Close(); err != nil {
		return nil, false, err
	}
	blob := &archiveBlob{digest: "sha256:" + hex.EncodeToString(hash.Sum(nil)), size: size}
	blob.magic = head.buf[:min(len(head.buf), 4)]
	if !head.overflow {
		blob.data = head.buf
	}
	target := filepath.Join(l.LayersDir(), blob.digest)
	if info, err := os.Stat(target); err == nil && info.Size() == size {
		return blob, false, nil
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return nil, false, err
	}
	l.chown(target)
	return blob, true, nil
}

// headBuffer keeps a member's bytes while it is small enough to be metadata,
// and its first bytes in any case for compression detection.
type headBuffer struct {
	buf      []byte
	limit    int
	overflow bool
}

func (h *headBuffer) Write(p []byte) (int, error) {
	if !h.overflow {
		if len(h.buf)+len(p) <= h.limit {
			h.buf = append(h.buf, p...)
		} else {
			h.overflow = true
			if len(h.buf) < 4 {
				h.buf = append(h.buf, p[:min(len(p), 4-len(h.buf))]...)
			}
			h.buf = h.buf[:min(len(h.buf), 4)]
		}
	}
	return len(p), nil
}

// resolveArchiveImage finds the image the archive carries for platform.
// broken lists the link members that resolve to no member, for the error.
func resolveArchiveImage(files map[string]*archiveBlob, broken map[string]error, ref Ref, platform Platform) (*Image, error) {
	if index, ok := files["index.json"]; ok && index.data != nil {
		image, err := resolveOCIIndex(files, index.data, ref, platform, 0)
		if err == nil {
			return image, nil
		}
		if _, hasDocker := files["manifest.json"]; !hasDocker {
			return nil, err
		}
	}
	docker, ok := files["manifest.json"]
	if !ok || docker.data == nil {
		return nil, fmt.Errorf("image archive has neither a usable index.json nor manifest.json")
	}
	return resolveDockerManifest(files, broken, docker.data, ref)
}

type ociIndex struct {
	Manifests []struct {
		MediaType string    `json:"mediaType"`
		Digest    string    `json:"digest"`
		Size      int64     `json:"size"`
		Platform  *Platform `json:"platform"`
	} `json:"manifests"`
}

func resolveOCIIndex(files map[string]*archiveBlob, data []byte, ref Ref, platform Platform, depth int) (*Image, error) {
	if depth > 4 {
		return nil, fmt.Errorf("image archive index nests too deeply")
	}
	var index ociIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("decode archive index: %w", err)
	}
	candidates := index.Manifests[:0:0]
	for _, entry := range index.Manifests {
		if entry.Platform == nil || (entry.Platform.OS == platform.OS && entry.Platform.Architecture == platform.Architecture) {
			candidates = append(candidates, entry)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("image archive has no manifest for %s/%s", platform.OS, platform.Architecture)
	}
	for _, entry := range candidates {
		blob := files["blobs/sha256/"+strings.TrimPrefix(entry.Digest, "sha256:")]
		if blob == nil || blob.data == nil {
			continue
		}
		if strings.Contains(entry.MediaType, "index") || strings.Contains(entry.MediaType, "manifest.list") {
			if image, err := resolveOCIIndex(files, blob.data, ref, platform, depth+1); err == nil {
				return image, nil
			}
			continue
		}
		var manifest Manifest
		if err := json.Unmarshal(blob.data, &manifest); err != nil || len(manifest.Layers) == 0 {
			continue
		}
		if !archiveHas(files, manifest) {
			// Docker omits the blobs of platforms it did not save.
			continue
		}
		return &Image{Ref: ref, Manifest: manifest, ManifestJSON: blob.data}, nil
	}
	return nil, fmt.Errorf("image archive index names no complete manifest for %s/%s", platform.OS, platform.Architecture)
}

func archiveHas(files map[string]*archiveBlob, manifest Manifest) bool {
	for _, desc := range append([]Descriptor{manifest.Config}, manifest.Layers...) {
		if files["blobs/sha256/"+strings.TrimPrefix(desc.Digest, "sha256:")] == nil {
			return false
		}
	}
	return true
}

// resolveDockerManifest reads docker-save's manifest.json. Its paths name
// either classic <id>/layer.tar members or OCI blobs; either way the stored
// content digests become a synthesized OCI manifest.
func resolveDockerManifest(files map[string]*archiveBlob, broken map[string]error, data []byte, ref Ref) (*Image, error) {
	var entries []struct {
		Config   string   `json:"Config"`
		RepoTags []string `json:"RepoTags"`
		Layers   []string `json:"Layers"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("decode archive manifest.json: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("archive manifest.json lists no image")
	}
	chosen := 0
	if len(entries) > 1 {
		chosen = -1
		want := ref.Local + ":" + ref.Tag
		for i, entry := range entries {
			for _, tag := range entry.RepoTags {
				if tag == want || "docker.io/"+tag == want || "docker.io/library/"+tag == want {
					chosen = i
				}
			}
		}
		if chosen < 0 {
			return nil, fmt.Errorf("archive holds %d images and none is tagged %s", len(entries), want)
		}
	}
	entry := entries[chosen]
	describe := func(name, mediaType string) (Descriptor, error) {
		blob := files[path.Clean(name)]
		if err := broken[path.Clean(name)]; blob == nil && err != nil {
			return Descriptor{}, fmt.Errorf("archive manifest.json names member %s, %w", name, err)
		}
		if blob == nil {
			return Descriptor{}, fmt.Errorf("archive manifest.json names missing member %s", name)
		}
		return Descriptor{MediaType: mediaType, Digest: blob.digest, Size: blob.size}, nil
	}
	config, err := describe(entry.Config, mediaOCIConfig)
	if err != nil {
		return nil, err
	}
	manifest := Manifest{SchemaVersion: 2, MediaType: mediaOCIManifest, Config: config}
	for _, name := range entry.Layers {
		layer, err := describe(name, "")
		if err != nil {
			return nil, err
		}
		layer.MediaType = layerMediaType(files[path.Clean(name)].magic)
		manifest.Layers = append(manifest.Layers, layer)
	}
	if len(manifest.Layers) == 0 {
		return nil, fmt.Errorf("archive image has no layers")
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	return &Image{Ref: ref, Manifest: manifest, ManifestJSON: encoded}, nil
}

func layerMediaType(magic []byte) string {
	switch {
	case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		return mediaLayerGzip
	case len(magic) >= 4 && magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd:
		return mediaLayerZstd
	default:
		return mediaLayerTar
	}
}

// verifyDiffIDs checks uncompressed layers against the config's rootfs
// diff_ids, the only integrity record a classic archive carries for them.
func verifyDiffIDs(files map[string]*archiveBlob, image *Image) error {
	var config *archiveBlob
	for _, blob := range files {
		if blob.digest == image.Manifest.Config.Digest && blob.data != nil {
			config = blob
			break
		}
	}
	if config == nil {
		return fmt.Errorf("image config %s missing from archive", shortDigest(image.Manifest.Config.Digest))
	}
	var parsed struct {
		RootFS struct {
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	if err := json.Unmarshal(config.data, &parsed); err != nil {
		return fmt.Errorf("decode image config: %w", err)
	}
	if len(parsed.RootFS.DiffIDs) != 0 && len(parsed.RootFS.DiffIDs) != len(image.Manifest.Layers) {
		return fmt.Errorf("image config lists %d layers but the archive has %d", len(parsed.RootFS.DiffIDs), len(image.Manifest.Layers))
	}
	for i, layer := range image.Manifest.Layers {
		if layer.MediaType == mediaLayerTar && i < len(parsed.RootFS.DiffIDs) && parsed.RootFS.DiffIDs[i] != layer.Digest {
			return fmt.Errorf("archive layer %d is corrupt: digest %s, config expects %s", i, shortDigest(layer.Digest), shortDigest(parsed.RootFS.DiffIDs[i]))
		}
	}
	return nil
}
