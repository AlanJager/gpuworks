package native

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// saveMembers saves image from layout and returns the archive with its
// members by name, failing on a duplicate member.
func saveMembers(t *testing.T, layout Layout, image string) ([]byte, map[string][]byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := layout.SaveArchive(context.Background(), ParseRef(image), &buf); err != nil {
		t.Fatalf("SaveArchive: %v", err)
	}
	members := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if _, dup := members[hdr.Name]; dup {
			t.Fatalf("member %s written twice", hdr.Name)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		members[hdr.Name] = data
	}
	return buf.Bytes(), members
}

func ociArchive(t *testing.T, p imageParts) ([]byte, []byte) {
	members := map[string][]byte{"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`)}
	manifest := ociBlobs(p, members)
	members["index.json"] = indexOf(map[string]any{"mediaType": mediaOCIManifest, "digest": "sha256:" + sha(manifest), "size": len(manifest)})
	return archive(t, members, nil), manifest
}

// The saved archive carries both indexes -- index.json for OCI loaders and
// manifest.json for docker's classic loader -- over the stored blobs verbatim,
// and loads back as the same image: the manifest digest, which is the image ID
// peer sync compares, survives the trip.
func TestSaveArchiveRoundTripKeepsImageID(t *testing.T) {
	p := newImageParts(t)
	data, manifest := ociArchive(t, p)
	const image = "registry.example/team/app:v1"
	_, source := loadAndCreate(t, data, image)

	saved, members := saveMembers(t, source, image)
	manifestDigest := "sha256:" + sha(manifest)
	if !bytes.Equal(members["blobs/sha256/"+sha(manifest)], manifest) {
		t.Fatal("manifest blob is not the stored manifest verbatim")
	}
	for i, layer := range p.gz {
		if !bytes.Equal(members["blobs/sha256/"+sha(layer)], layer) {
			t.Fatalf("layer %d was not streamed as stored", i)
		}
	}
	if string(members["oci-layout"]) != `{"imageLayoutVersion":"1.0.0"}` {
		t.Fatalf("oci-layout = %q", members["oci-layout"])
	}
	var index struct {
		SchemaVersion int `json:"schemaVersion"`
		Manifests     []struct {
			MediaType   string            `json:"mediaType"`
			Digest      string            `json:"digest"`
			Size        int               `json:"size"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(members["index.json"], &index); err != nil {
		t.Fatal(err)
	}
	if index.SchemaVersion != 2 || len(index.Manifests) != 1 {
		t.Fatalf("index.json = %s", members["index.json"])
	}
	entry := index.Manifests[0]
	if entry.MediaType != mediaOCIManifest || entry.Digest != manifestDigest || entry.Size != len(manifest) ||
		entry.Annotations["io.containerd.image.name"] != image || entry.Annotations["org.opencontainers.image.ref.name"] != "v1" {
		t.Fatalf("index entry = %+v", entry)
	}
	var docker []struct {
		Config   string
		RepoTags []string
		Layers   []string
	}
	if err := json.Unmarshal(members["manifest.json"], &docker); err != nil {
		t.Fatal(err)
	}
	if len(docker) != 1 || docker[0].Config != "blobs/sha256/"+sha(p.config) ||
		len(docker[0].RepoTags) != 1 || docker[0].RepoTags[0] != image ||
		len(docker[0].Layers) != 2 || docker[0].Layers[1] != "blobs/sha256/"+sha(p.gz[1]) {
		t.Fatalf("manifest.json = %s", members["manifest.json"])
	}
	// config + two layers + manifest, and the three index files: nothing else.
	if len(members) != 7 {
		t.Fatalf("unexpected members: %d", len(members))
	}

	// A Docker Hub name is fully qualified for containerd, familiar for docker.
	_, hub := loadAndCreate(t, data, "team/app:v1")
	_, hubMembers := saveMembers(t, hub, "team/app:v1")
	if !strings.Contains(string(hubMembers["index.json"]), `"io.containerd.image.name":"docker.io/team/app:v1"`) ||
		!strings.Contains(string(hubMembers["manifest.json"]), `"RepoTags":["team/app:v1"]`) {
		t.Fatalf("Docker Hub names: %s %s", hubMembers["index.json"], hubMembers["manifest.json"])
	}

	// The receiving node may register the image under another reference.
	_, target := loadAndCreate(t, saved, "mirror.example/app:v1")
	sourceID, _ := source.ImageID(ParseRef(image))
	targetID, _ := target.ImageID(ParseRef("mirror.example/app:v1"))
	if sourceID != manifestDigest || targetID != sourceID {
		t.Fatalf("image ID changed across save/load: %s -> %s", sourceID, targetID)
	}
}

// An image loaded from a classic docker save has a manifest this node wrote
// itself, over uncompressed layers; it must save just as well.
func TestSaveArchiveOfClassicLoadedImage(t *testing.T) {
	p := newImageParts(t)
	_, source := loadAndCreate(t, classicArchive(t, p, "team/app:v1"), "team/app:v1")
	saved, members := saveMembers(t, source, "team/app:v1")
	for i, layer := range p.plain {
		if !bytes.Equal(members["blobs/sha256/"+sha(layer)], layer) {
			t.Fatalf("layer %d missing from the archive", i)
		}
	}
	_, target := loadAndCreate(t, saved, "team/app:v1")
	sourceID, _ := source.ImageID(ParseRef("team/app:v1"))
	targetID, _ := target.ImageID(ParseRef("team/app:v1"))
	if sourceID == "" || targetID != sourceID {
		t.Fatalf("image ID changed across save/load: %s -> %s", sourceID, targetID)
	}
}

// A layer the manifest lists twice is one blob in the archive.
func TestSaveArchiveWritesRepeatedLayersOnce(t *testing.T) {
	p := newImageParts(t)
	var config map[string]any
	_ = json.Unmarshal(p.config, &config)
	rootfs := config["rootfs"].(map[string]any)
	diffIDs := rootfs["diff_ids"].([]any)
	rootfs["diff_ids"] = append(diffIDs, diffIDs[0])
	p.config, _ = json.Marshal(config)
	p.plain = append(p.plain, p.plain[0])
	p.gz = append(p.gz, p.gz[0])
	data, _ := ociArchive(t, p)
	_, source := loadAndCreate(t, data, "team/app:v1")

	saved, members := saveMembers(t, source, "team/app:v1") // fails on a duplicate member
	var docker []struct{ Layers []string }
	_ = json.Unmarshal(members["manifest.json"], &docker)
	if len(docker) != 1 || len(docker[0].Layers) != 3 || docker[0].Layers[0] != docker[0].Layers[2] {
		t.Fatalf("manifest.json must keep the repeated layer: %s", members["manifest.json"])
	}
	loadAndCreate(t, saved, "team/app:v1")
}

func TestSaveArchiveFailures(t *testing.T) {
	p := newImageParts(t)
	data, _ := ociArchive(t, p)
	const image = "team/app:v1"

	t.Run("unregistered image", func(t *testing.T) {
		_, layout := loadAndCreate(t, data, image)
		var buf bytes.Buffer
		err := layout.SaveArchive(context.Background(), ParseRef("team/other:v1"), &buf)
		if err == nil || !strings.Contains(err.Error(), "not in the native image store") || buf.Len() != 0 {
			t.Fatalf("err=%v, wrote %d bytes", err, buf.Len())
		}
	})
	for name, damage := range map[string]func(Layout) error{
		"truncated layer": func(l Layout) error { return os.Truncate(filepath.Join(l.LayersDir(), "sha256:"+sha(p.gz[1])), 3) },
		"missing config":  func(l Layout) error { return os.Remove(filepath.Join(l.LayersDir(), "sha256:"+sha(p.config))) },
	} {
		t.Run(name, func(t *testing.T) {
			_, layout := loadAndCreate(t, data, image)
			if err := damage(layout); err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			err := layout.SaveArchive(context.Background(), ParseRef(image), &buf)
			if err == nil || !strings.Contains(err.Error(), "not in the native image store") || buf.Len() != 0 {
				t.Fatalf("an incomplete image must not be sent: err=%v, wrote %d bytes", err, buf.Len())
			}
		})
	}
	// A blob that changes after the completeness check (a concurrent removal)
	// is caught while streaming rather than sent short.
	t.Run("blob changed while saving", func(t *testing.T) {
		_, layout := loadAndCreate(t, data, image)
		desc := Descriptor{Digest: "sha256:" + sha(p.gz[0]), Size: int64(len(p.gz[0])) + 1}
		err := layout.writeBlob(tar.NewWriter(io.Discard), "blobs/sha256/x", desc, time.Unix(0, 0))
		if err == nil || !strings.Contains(err.Error(), "manifest says") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		_, layout := loadAndCreate(t, data, image)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := layout.SaveArchive(ctx, ParseRef(image), io.Discard); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("writer error", func(t *testing.T) {
		_, layout := loadAndCreate(t, data, image)
		boom := errors.New("peer went away")
		if err := layout.SaveArchive(context.Background(), ParseRef(image), failingWriter{boom}); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }
