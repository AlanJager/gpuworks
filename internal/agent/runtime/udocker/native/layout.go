package native

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Layout is a UDOCKER_DIR tree. Everything this package writes has to be
// exactly what udocker's own localrepo would have written, because
// `udocker setup --nvidia`, `udocker setup --execmode=P1` and `udocker run`
// keep reading it afterwards.
//
//	<dir>/layers/sha256:<hex>              blob content
//	<dir>/repos/<imagerepo>/<tag>/TAG      "<reposdir>/<imagerepo>:<tag>"
//	<dir>/repos/<imagerepo>/<tag>/v2       empty marker selecting the v2 reader
//	<dir>/repos/<imagerepo>/<tag>/manifest the manifest bytes, verbatim
//	<dir>/repos/<imagerepo>/<tag>/sha256:… symlink per blob into ../layers
//	<dir>/containers/<id>/ROOT             extracted rootfs
//	<dir>/containers/<id>/container.json   the image config blob
//	<dir>/containers/<id>/imagerepo.name   "<imagerepo>:<tag>"
type Layout struct {
	Dir   string
	Owner *Owner
}

// CreateStagingSuffix marks the directory an in-flight Create builds before
// renaming it into place. Anything that enumerates containers/ has to ignore
// it: a staging directory already contains a ROOT but is not yet a container,
// so treating it as one would delete a rootfs that is still being written.
const CreateStagingSuffix = ".creating"

func (l Layout) LayersDir() string     { return filepath.Join(l.Dir, "layers") }
func (l Layout) ReposDir() string      { return filepath.Join(l.Dir, "repos") }
func (l Layout) ContainersDir() string { return filepath.Join(l.Dir, "containers") }

func (l Layout) TagDir(ref Ref) string {
	return filepath.Join(l.ReposDir(), filepath.FromSlash(ref.Local), ref.Tag)
}

func (l Layout) ContainerDir(name string) string {
	return filepath.Join(l.ContainersDir(), name)
}

// HasImage reports whether a tag directory is registered and every blob its
// manifest references is present, which is the same condition udocker's
// cd_imagerepo plus get_image_attributes check before a create.
func (l Layout) HasImage(ref Ref) bool {
	tagDir := l.TagDir(ref)
	if _, err := os.Stat(filepath.Join(tagDir, "TAG")); err != nil {
		return false
	}
	manifest, err := l.LoadManifest(ref)
	if err != nil {
		return false
	}
	for _, desc := range append([]Descriptor{manifest.Config}, manifest.Layers...) {
		info, err := os.Stat(filepath.Join(l.LayersDir(), desc.Digest))
		if err != nil || info.Size() != desc.Size {
			return false
		}
	}
	return true
}

// LoadManifest reads back a previously registered manifest.
func (l Layout) LoadManifest(ref Ref) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(l.TagDir(ref), "manifest"))
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, err
	}
	if len(manifest.Layers) == 0 {
		return Manifest{}, fmt.Errorf("stored manifest for %s has no layers", ref.Local)
	}
	return manifest, nil
}

// ImageID returns the content identity of a registered manifest. The registry
// digest is the sha256 of the exact manifest bytes, which is stable across
// tags and suitable for the server's immutable image snapshot.
func (l Layout) ImageID(ref Ref) (string, error) {
	data, err := os.ReadFile(filepath.Join(l.TagDir(ref), "manifest"))
	if err != nil {
		return "", err
	}
	return digestOf(data), nil
}

// digestOf is the content digest of data in "sha256:<hex>" form.
func digestOf(data []byte) string {
	hash := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", hash[:])
}

// RegisterImage writes the repository metadata for an image whose blobs are
// already in the layer store. It is the last step of a pull so that a crash
// mid-download never leaves a tag directory that looks complete.
func (l Layout) RegisterImage(image *Image) error {
	tagDir := l.TagDir(image.Ref)
	if err := os.MkdirAll(tagDir, 0o755); err != nil {
		return err
	}
	// udocker writes the absolute repository path followed by ":<tag>".
	tagValue := filepath.Join(l.ReposDir(), filepath.FromSlash(image.Ref.Local)) + ":" + image.Ref.Tag
	if err := l.writeFile(filepath.Join(tagDir, "TAG"), []byte(tagValue)); err != nil {
		return err
	}
	if err := l.writeFile(filepath.Join(tagDir, "v2"), nil); err != nil {
		return err
	}
	if err := l.writeFile(filepath.Join(tagDir, "manifest"), image.ManifestJSON); err != nil {
		return err
	}
	// The per-blob symlinks are what get_image_attributes resolves to find the
	// layer files; their depth depends on how deep the repository name is.
	// repos/<a>/<b>/<tag>/ is len(components)+2 levels below UDOCKER_DIR.
	depth := len(strings.Split(filepath.ToSlash(image.Ref.Local), "/")) + 2
	prefix := strings.Repeat("../", depth) + "layers/"
	for _, desc := range append([]Descriptor{image.Manifest.Config}, image.Manifest.Layers...) {
		link := filepath.Join(tagDir, desc.Digest)
		_ = os.Remove(link)
		if err := os.Symlink(prefix+desc.Digest, link); err != nil {
			return err
		}
		l.chown(link)
	}
	l.chownTree(filepath.Join(l.ReposDir(), filepath.FromSlash(image.Ref.Local)))
	return nil
}

// RegisterContainer writes the container metadata beside an already extracted
// ROOT so udocker recognizes it as one of its own.
func (l Layout) RegisterContainer(name string, ref Ref, config []byte) error {
	dir := l.ContainerDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := l.writeFile(filepath.Join(dir, "container.json"), config); err != nil {
		return err
	}
	if err := l.writeFile(filepath.Join(dir, "imagerepo.name"), []byte(ref.Local+":"+ref.Tag)); err != nil {
		return err
	}
	l.chown(dir)
	return nil
}

func (l Layout) writeFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	l.chown(path)
	return nil
}

func (l Layout) chown(path string) {
	if l.Owner != nil {
		_ = os.Lchown(path, l.Owner.UID, l.Owner.GID)
	}
}

func (l Layout) chownTree(root string) {
	if l.Owner == nil {
		return
	}
	_ = filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err == nil {
			_ = os.Lchown(path, l.Owner.UID, l.Owner.GID)
		}
		return nil
	})
}
