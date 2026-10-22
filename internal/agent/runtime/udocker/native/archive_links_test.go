package native

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// v1Layer is one layer directory of a classic `docker save` archive: a regular
// layer.tar, or one that links to another layer directory's.
type v1Layer struct {
	id      string
	body    []byte
	symlink string // layer.tar -> symlink, as Docker before 25 writes repeats
	hard    string // layer.tar hard-linked to this archive path, as GNU tar -h writes
}

// docker19Save renders what `docker save` of Docker 19/20 writes: it tars a
// directory holding <id>/{VERSION,json,layer.tar}, <config sha>.json,
// manifest.json and repositories, in filepath.Walk (lexical) order. A layer
// whose diff_id already appeared is not stored again: its layer.tar is a
// symlink "../<first id>/layer.tar". Real listing from Docker 19.03.15:
//
//	lrwxrwxrwx 0/0 0 bfdb3034…/layer.tar -> ../aa6cb31a…/layer.tar
//
// prefix "./" reproduces `tar -C dir -c .` repacks.
func docker19Save(t *testing.T, config []byte, tag string, layers []v1Layer, extra []entry, prefix string) []byte {
	t.Helper()
	members := map[string]entry{}
	add := func(e entry) { members[e.name] = e }
	add(entry{name: sha(config) + ".json", body: string(config)})
	var paths []string
	parent := ""
	for _, layer := range layers {
		add(entry{name: layer.id + "/", dir: true})
		add(entry{name: layer.id + "/VERSION", body: "1.0"})
		legacy, _ := json.Marshal(map[string]any{"id": layer.id, "parent": parent, "created": "2026-10-18T00:17:00Z", "os": "linux"})
		add(entry{name: layer.id + "/json", body: string(legacy)})
		tarEntry := entry{name: layer.id + "/layer.tar", body: string(layer.body)}
		switch {
		case layer.symlink != "":
			tarEntry = entry{name: tarEntry.name, symlink: layer.symlink, mode: 0o777}
		case layer.hard != "":
			tarEntry = entry{name: tarEntry.name, linkname: prefix + layer.hard}
		}
		add(tarEntry)
		paths = append(paths, layer.id+"/layer.tar")
		parent = layer.id
	}
	manifest, _ := json.Marshal([]map[string]any{{"Config": sha(config) + ".json", "RepoTags": []string{tag}, "Layers": paths}})
	add(entry{name: "manifest.json", body: string(manifest)})
	repo, version, _ := strings.Cut(tag, ":")
	repositories, _ := json.Marshal(map[string]map[string]string{repo: {version: parent}})
	add(entry{name: "repositories", body: string(repositories)})
	for _, e := range extra {
		add(e)
	}
	var names []string
	for name := range members {
		names = append(names, name)
	}
	sort.Strings(names)
	var ordered []entry
	for _, name := range names {
		e := members[name]
		e.name = prefix + e.name
		ordered = append(ordered, e)
	}
	return buildLayer(t, false, ordered)
}

// threeLayerConfig is a config for layers plain[0], plain[1], plain[0]: the
// first layer's content appears twice, as with two identical COPY steps.
func threeLayerConfig(t *testing.T, p imageParts) []byte {
	t.Helper()
	config, _ := json.Marshal(map[string]any{
		"architecture": "amd64", "os": "linux",
		"config": map[string]any{"Entrypoint": []string{"/app/run"}},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{
			"sha256:" + sha(p.plain[0]), "sha256:" + sha(p.plain[1]), "sha256:" + sha(p.plain[0]),
		}},
	})
	return config
}

var (
	v1First  = "aa6cb31abd97d95099ba79c204723a3cd25279e1354df5a65e942c4fbac347da"
	v1Second = "5f1e0a3cbd8f4b0b6f8b1c46d2f1a6c7e0e4d2b1a3c5e7f9b2d4f6a8c0e2a4c6"
	v1Repeat = "bfdb30348fb4c7b6c69dcda344bde9eb87ae4a2ae9a474b4a704bb72911c35a2"
	// Sorts before v1First, so the link is read before its target.
	v1EarlyRepeat = "0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9"
)

func assertRepeatedLayer(t *testing.T, image *Image, layout Layout, p imageParts) {
	t.Helper()
	want := []string{"sha256:" + sha(p.plain[0]), "sha256:" + sha(p.plain[1]), "sha256:" + sha(p.plain[0])}
	if len(image.Manifest.Layers) != len(want) {
		t.Fatalf("layers = %+v", image.Manifest.Layers)
	}
	for i, layer := range image.Manifest.Layers {
		if layer.Digest != want[i] || layer.MediaType != mediaLayerTar || layer.Size != int64(len(p.plain[i%2])) {
			t.Fatalf("layer %d = %+v, want digest %s", i, layer, want[i])
		}
	}
	// The repeated layer is the same blob: config + two layers.
	got := layerFiles(t, layout)
	wantFiles := []string{"sha256:" + sha(p.plain[0]), "sha256:" + sha(p.plain[1]), image.Manifest.Config.Digest}
	sort.Strings(wantFiles)
	if strings.Join(got, ",") != strings.Join(wantFiles, ",") {
		t.Fatalf("layer store = %v, want %v", got, wantFiles)
	}
	for _, digest := range want[:2] {
		data, err := os.ReadFile(filepath.Join(layout.LayersDir(), digest))
		if err != nil || "sha256:"+sha(data) != digest {
			t.Fatalf("blob %s holds other content: %v", digest, err)
		}
	}
}

// The T70 archive shape: Docker 19.03 stored the repeated layer as a symlink
// and the import failed with "names missing member <id>/layer.tar".
func TestLoadDocker19ArchiveWithSymlinkedLayer(t *testing.T) {
	p := newImageParts(t)
	data := docker19Save(t, threeLayerConfig(t, p), "team/app:v1", []v1Layer{
		{id: v1First, body: p.plain[0]},
		{id: v1Second, body: p.plain[1]},
		{id: v1Repeat, symlink: "../" + v1First + "/layer.tar"},
	}, nil, "")
	image, layout := loadAndCreate(t, data, "team/app:v1")
	assertRepeatedLayer(t, image, layout, p)
}

func TestLoadArchiveSymlinkReadBeforeItsTarget(t *testing.T) {
	p := newImageParts(t)
	data := docker19Save(t, threeLayerConfig(t, p), "team/app:v1", []v1Layer{
		{id: v1First, body: p.plain[0]},
		{id: v1Second, body: p.plain[1]},
		{id: v1EarlyRepeat, symlink: "../" + v1First + "/layer.tar"},
	}, nil, "")
	image, layout := loadAndCreate(t, data, "team/app:v1")
	assertRepeatedLayer(t, image, layout, p)
}

// GNU tar -h over an extracted Docker 19 archive dereferences the symlink, and
// the second sighting of the same inode becomes a hard link to the first.
func TestLoadRepackedArchiveWithHardLinkedLayer(t *testing.T) {
	p := newImageParts(t)
	data := docker19Save(t, threeLayerConfig(t, p), "team/app:v1", []v1Layer{
		{id: v1First, body: p.plain[0]},
		{id: v1Second, body: p.plain[1]},
		{id: v1Repeat, hard: v1First + "/layer.tar"},
	}, nil, "./")
	image, layout := loadAndCreate(t, data, "team/app:v1")
	assertRepeatedLayer(t, image, layout, p)
}

func TestLoadArchiveFollowsLinkChains(t *testing.T) {
	p := newImageParts(t)
	middle := strings.Repeat("c", 64)
	data := docker19Save(t, threeLayerConfig(t, p), "team/app:v1", []v1Layer{
		{id: v1First, body: p.plain[0]},
		{id: v1Second, body: p.plain[1]},
		{id: v1EarlyRepeat, symlink: "../" + middle + "/layer.tar"},
	}, []entry{
		{name: middle + "/", dir: true},
		{name: middle + "/layer.tar", linkname: v1First + "/layer.tar"},
	}, "")
	image, layout := loadAndCreate(t, data, "team/app:v1")
	assertRepeatedLayer(t, image, layout, p)
}

// Links the image does not use are not its business, even broken ones.
func TestLoadArchiveIgnoresUnusedBrokenLinks(t *testing.T) {
	p := newImageParts(t)
	data := docker19Save(t, threeLayerConfig(t, p), "team/app:v1", []v1Layer{
		{id: v1First, body: p.plain[0]},
		{id: v1Second, body: p.plain[1]},
		{id: v1Repeat, symlink: "../" + v1First + "/layer.tar"},
	}, []entry{
		{name: "dangling/layer.tar", symlink: "../gone/layer.tar", mode: 0o777},
		{name: "outside", symlink: "/etc/passwd", mode: 0o777},
		{name: "loop-a", symlink: "loop-b", mode: 0o777},
		{name: "loop-b", symlink: "loop-a", mode: 0o777},
	}, "")
	image, layout := loadAndCreate(t, data, "team/app:v1")
	assertRepeatedLayer(t, image, layout, p)
}

func TestLoadArchiveRejectsBadLinks(t *testing.T) {
	p := newImageParts(t)
	config := threeLayerConfig(t, p)
	// The repeated layer's layer.tar is replaced by link.
	repeatAs := func(link v1Layer, extra ...entry) []byte {
		link.id = v1Repeat
		return docker19Save(t, config, "team/app:v1", []v1Layer{
			{id: v1First, body: p.plain[0]},
			{id: v1Second, body: p.plain[1]},
			link,
		}, extra, "")
	}
	other := strings.Repeat("f", 64)
	blob := sha(p.plain[1])
	cases := map[string]struct {
		data []byte
		want string
	}{
		"symlink to a missing member": {
			repeatAs(v1Layer{symlink: "../" + other + "/layer.tar"}),
			"names member " + v1Repeat + "/layer.tar, a link to missing member " + other + "/layer.tar",
		},
		"hard link to a missing member": {
			repeatAs(v1Layer{hard: other + "/layer.tar"}),
			"a link to missing member " + other + "/layer.tar",
		},
		"symlink climbing out of the archive": {
			repeatAs(v1Layer{symlink: "../../" + v1First + "/layer.tar"}),
			"a link that points outside the archive",
		},
		"absolute symlink": {
			repeatAs(v1Layer{symlink: "/etc/passwd"}),
			"a link that points outside the archive",
		},
		"hard link climbing out of the archive": {
			repeatAs(v1Layer{hard: "../" + v1First + "/layer.tar"}),
			"a link that points outside the archive",
		},
		"link cycle": {
			repeatAs(v1Layer{symlink: "../" + other + "/layer.tar"},
				entry{name: other + "/", dir: true},
				entry{name: other + "/layer.tar", symlink: "../" + v1Repeat + "/layer.tar", mode: 0o777}),
			"(a cycle)",
		},
		"blob path linking to other content": {
			repeatAs(v1Layer{symlink: "../" + v1First + "/layer.tar"},
				entry{name: "blobs/sha256/" + blob, symlink: "../../" + v1First + "/layer.tar", mode: 0o777}),
			"archive blob blobs/sha256/" + blob + " is corrupt",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			layout := Layout{Dir: t.TempDir()}
			_, err := layout.LoadArchive(context.Background(), ParseRef("team/app:v1"), amd64, bytes.NewReader(tc.data))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
			if got := layerFiles(t, layout); len(got) != 0 {
				t.Fatalf("failed load left files in the layer store: %v", got)
			}
			if _, err := os.Stat(layout.ReposDir()); !os.IsNotExist(err) {
				t.Fatalf("failed load registered something: %v", err)
			}
		})
	}
}
