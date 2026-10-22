package native

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	kgzip "github.com/klauspost/compress/gzip"
)

// entry is one tar member in a synthetic layer.
type entry struct {
	name     string
	body     string
	dir      bool
	symlink  string
	linkname string // hard link target
	mode     int64
	pad      int  // extra bytes appended to body, to make a layer "large"
	typeflag byte // overrides the inferred type when set
}

// buildLayer renders entries as a tar stream, gzipped when compress is set.
func buildLayer(t *testing.T, compress bool, entries []entry) []byte {
	t.Helper()
	var raw bytes.Buffer
	writer := tar.NewWriter(&raw)
	for _, e := range entries {
		header := &tar.Header{Name: e.name, Mode: 0o644}
		if e.mode != 0 {
			header.Mode = e.mode
		}
		body := e.body
		if e.pad > 0 {
			body += strings.Repeat("x", e.pad)
		}
		switch {
		case e.dir:
			header.Typeflag = tar.TypeDir
			header.Mode = 0o755
			if e.mode != 0 {
				header.Mode = e.mode
			}
		case e.symlink != "":
			header.Typeflag = tar.TypeSymlink
			header.Linkname = e.symlink
		case e.linkname != "":
			header.Typeflag = tar.TypeLink
			header.Linkname = e.linkname
		case e.typeflag == tar.TypeXGlobalHeader:
			header = &tar.Header{
				Name:       e.name,
				Typeflag:   tar.TypeXGlobalHeader,
				PAXRecords: map[string]string{"comment": "global"},
				Format:     tar.FormatPAX,
			}
		case e.typeflag != 0:
			header.Typeflag = e.typeflag
		default:
			header.Typeflag = tar.TypeReg
			header.Size = int64(len(body))
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := io.WriteString(writer, body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if !compress {
		return raw.Bytes()
	}
	var gz bytes.Buffer
	zw := kgzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return gz.Bytes()
}

// unpackLayers runs the Unpacker over synthetic layers, alternating gzip and
// plain so both stream paths are exercised in every test.
func unpackLayers(t *testing.T, workers int, layerEntries ...[]entry) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "ROOT")
	sources := make([]LayerSource, 0, len(layerEntries))
	for i, entries := range layerEntries {
		data := buildLayer(t, i%2 == 0, entries)
		sources = append(sources, LayerSource{
			Digest: digestFor(i),
			Size:   int64(len(data)),
			Open: func(context.Context) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(data)), nil
			},
		})
	}
	// Images legitimately contain read-only directories; make the tree
	// writable again so t.TempDir's cleanup can remove it.
	t.Cleanup(func() {
		filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	unpacker := &Unpacker{Root: root, Workers: workers}
	if err := unpacker.Unpack(context.Background(), sources); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	return root
}

func digestFor(i int) string {
	return "sha256:" + strings.Repeat(string(rune('a'+i)), 64)
}

func readFile(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func mustNotExist(t *testing.T, root, name string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name))); !os.IsNotExist(err) {
		t.Errorf("%s exists, want removed (err=%v)", name, err)
	}
}

func mustExist(t *testing.T, root, name string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name))); err != nil {
		t.Errorf("%s missing: %v", name, err)
	}
}

func TestUnpackHigherLayerWinsRegardlessOfExecutionOrder(t *testing.T) {
	// Layer 0 is padded so the size-descending schedule starts it first while
	// layer 1 must still win the shared path.
	root := unpackLayers(t, 4,
		[]entry{{name: "etc/conf", body: "old"}, {name: "etc/only-lower", body: "keep"}, {name: "big", body: "pad", pad: 1 << 16}},
		[]entry{{name: "etc/conf", body: "new"}},
	)
	if got := readFile(t, root, "etc/conf"); got != "new" {
		t.Errorf("etc/conf = %q, want %q from the higher layer", got, "new")
	}
	if got := readFile(t, root, "etc/only-lower"); got != "keep" {
		t.Errorf("etc/only-lower = %q, want %q", got, "keep")
	}
}

func TestUnpackPlainWhiteoutRemovesLowerLayerPath(t *testing.T) {
	root := unpackLayers(t, 4,
		[]entry{{name: "usr/lib/libfoo.so", body: "old"}, {name: "usr/lib/libbar.so", body: "keep"}},
		[]entry{{name: "usr/lib/.wh.libfoo.so"}},
	)
	mustNotExist(t, root, "usr/lib/libfoo.so")
	mustNotExist(t, root, "usr/lib/.wh.libfoo.so") // the marker is never extracted
	if got := readFile(t, root, "usr/lib/libbar.so"); got != "keep" {
		t.Errorf("usr/lib/libbar.so = %q, want it untouched", got)
	}
}

// An opaque marker hides what lower layers put in the directory, but it always
// travels with its own layer's fresh contents for that directory. Deleting
// those too was a real bug: whole subtrees vanished from the rootfs.
func TestUnpackOpaqueWhiteoutKeepsItsOwnLayerContents(t *testing.T) {
	root := unpackLayers(t, 4,
		[]entry{
			{name: "etc/fonts", dir: true},
			{name: "etc/fonts/stale.conf", body: "stale"},
			{name: "etc/fonts/sub", dir: true},
			{name: "etc/fonts/sub/deep.conf", body: "stale"},
			{name: "etc/keep.conf", body: "keep"},
		},
		[]entry{
			{name: "etc/fonts/.wh..wh..opq"},
			{name: "etc/fonts", dir: true},
			{name: "etc/fonts/fresh.conf", body: "fresh"},
		},
	)
	mustNotExist(t, root, "etc/fonts/stale.conf")
	mustNotExist(t, root, "etc/fonts/sub")
	if got := readFile(t, root, "etc/fonts/fresh.conf"); got != "fresh" {
		t.Errorf("etc/fonts/fresh.conf = %q, want the opaque layer's own content preserved", got)
	}
	if got := readFile(t, root, "etc/keep.conf"); got != "keep" {
		t.Errorf("etc/keep.conf = %q, want files outside the opaque directory untouched", got)
	}
}

func TestUnpackHigherLayerRestoresWhiteoutPath(t *testing.T) {
	root := unpackLayers(t, 4,
		[]entry{{name: "opt/tool", body: "v1"}},
		[]entry{{name: "opt/.wh.tool"}},
		[]entry{{name: "opt/tool", body: "v3"}},
	)
	if got := readFile(t, root, "opt/tool"); got != "v3" {
		t.Errorf("opt/tool = %q, want %q: a layer above the whiteout must win", got, "v3")
	}
}

func TestUnpackAppliesSymlinksAndHardLinks(t *testing.T) {
	root := unpackLayers(t, 4,
		[]entry{
			{name: "usr/bin/real", body: "payload"},
			{name: "usr/bin/alias", symlink: "real"},
		},
		[]entry{{name: "usr/bin/hard", linkname: "usr/bin/real"}},
	)
	target, err := os.Readlink(filepath.Join(root, "usr/bin/alias"))
	if err != nil || target != "real" {
		t.Errorf("readlink usr/bin/alias = %q, %v; want %q", target, err, "real")
	}
	if got := readFile(t, root, "usr/bin/hard"); got != "payload" {
		t.Errorf("usr/bin/hard = %q, want the link target's content", got)
	}
}

func TestUnpackPreservesAlternativesStyleLinks(t *testing.T) {
	root := unpackLayers(t, 4,
		[]entry{
			{name: "etc/alternatives/cuda", symlink: "/usr/local/cuda-13.0"},
			{name: "etc/alternatives/c++", symlink: "/usr/bin/g++-12"},
			{name: "etc/alternatives/python3", symlink: "/usr/bin/python3.12"},
			{name: "usr/local/cuda", symlink: "/etc/alternatives/cuda"},
			{name: "usr/bin/c++", symlink: "/etc/alternatives/c++"},
			{name: "usr/bin/python3", symlink: "/etc/alternatives/python3"},
		},
	)
	for name, want := range map[string]string{
		"etc/alternatives/cuda":    "/usr/local/cuda-13.0",
		"etc/alternatives/c++":     "/usr/bin/g++-12",
		"etc/alternatives/python3": "/usr/bin/python3.12",
		"usr/local/cuda":           "/etc/alternatives/cuda",
		"usr/bin/c++":              "/etc/alternatives/c++",
		"usr/bin/python3":          "/etc/alternatives/python3",
	} {
		got, err := os.Readlink(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil || got != want {
			t.Errorf("%s -> %q, err=%v; want %q", name, got, err, want)
		}
	}
}

// A symlink replacing a directory (and the reverse) is how images relocate
// trees such as /lib -> /usr/lib between layers.
func TestUnpackReplacesDirectoryWithSymlink(t *testing.T) {
	root := unpackLayers(t, 4,
		[]entry{{name: "lib", dir: true}, {name: "lib/old.so", body: "old"}},
		[]entry{{name: "lib", symlink: "usr/lib"}},
	)
	info, err := os.Lstat(filepath.Join(root, "lib"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("lib mode = %v, want a symlink replacing the lower layer's directory", info.Mode())
	}
}

// udocker passes tar --exclude=dev/*, and GNU tar applies that pattern
// unanchored, so it silently drops any nested directory named "dev" as well.
// Only the rootfs-level /dev may be skipped.
func TestUnpackExcludesOnlyRootLevelDev(t *testing.T) {
	root := unpackLayers(t, 4, []entry{
		{name: "dev/null", body: "excluded"},
		{name: "usr/lib/python3/site-packages/pkg/dev/api.py", body: "kept"},
		{name: "etc/udev/devices/marker", body: "excluded"},
	})
	mustNotExist(t, root, "dev/null")
	mustNotExist(t, root, "etc/udev/devices/marker")
	if got := readFile(t, root, "usr/lib/python3/site-packages/pkg/dev/api.py"); got != "kept" {
		t.Errorf("nested dev/api.py = %q, want it kept", got)
	}
}

func TestUnpackRejectsPathEscape(t *testing.T) {
	data := buildLayer(t, false, []entry{{name: "../escape", body: "x"}})
	root := filepath.Join(t.TempDir(), "ROOT")
	unpacker := &Unpacker{Root: root, Workers: 1}
	err := unpacker.Unpack(context.Background(), []LayerSource{{
		Digest: digestFor(0),
		Size:   int64(len(data)),
		Open:   func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
	}})
	if err == nil {
		t.Fatal("Unpack accepted a layer whose entry escapes the destination")
	}
	if !strings.Contains(err.Error(), "escapes") {
		t.Errorf("error = %v, want it to name the escape", err)
	}
}

// Backslash is a legal character in a Linux filename; systemd unit names use
// it. Treating it as a separator split such entries into directories.
func TestUnpackKeepsBackslashInNames(t *testing.T) {
	root := unpackLayers(t, 2, []entry{
		{name: `usr/lib/systemd/system/system-systemd\x2dcryptsetup.slice`, body: "unit"},
	})
	if got := readFile(t, root, `usr/lib/systemd/system/system-systemd\x2dcryptsetup.slice`); got != "unit" {
		t.Errorf("systemd slice = %q, want the backslash preserved in the filename", got)
	}
}

// Layers routinely omit parent directory entries and rely on tar creating them.
func TestUnpackCreatesImplicitParents(t *testing.T) {
	root := unpackLayers(t, 2, []entry{{name: "a/b/c/d/file", body: "deep"}})
	if got := readFile(t, root, "a/b/c/d/file"); got != "deep" {
		t.Errorf("a/b/c/d/file = %q, want the implicit parents created", got)
	}
}

// A directory the image marks read-only must not block writes into it during
// the unpack, which is why udocker passes tar --delay-directory-restore.
func TestUnpackDefersReadOnlyDirectoryModes(t *testing.T) {
	root := unpackLayers(t, 2, []entry{
		{name: "ro", dir: true, mode: 0o500},
		{name: "ro/inside", body: "written"},
	})
	if got := readFile(t, root, "ro/inside"); got != "written" {
		t.Errorf("ro/inside = %q, want it written before the mode was restored", got)
	}
	info, err := os.Stat(filepath.Join(root, "ro"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o500 {
		t.Errorf("ro mode = %v, want 0500 restored after extraction", info.Mode().Perm())
	}
}

func TestUnpackIgnoresPaxHeaders(t *testing.T) {
	root := unpackLayers(t, 2, []entry{
		{name: "pax_global_header", typeflag: tar.TypeXGlobalHeader},
		{name: "etc/conf", body: "value"},
	})
	mustNotExist(t, root, "pax_global_header")
	mustExist(t, root, "etc/conf")
}

func TestUnpackSkipsDeviceNodes(t *testing.T) {
	// Device nodes need privileges udocker never has; they must be skipped
	// rather than failing the unpack.
	root := unpackLayers(t, 2, []entry{
		{name: "usr/share/dev-node", typeflag: tar.TypeChar},
		{name: "etc/conf", body: "value"},
	})
	mustNotExist(t, root, "usr/share/dev-node")
	mustExist(t, root, "etc/conf")
}

// Whatever the worker count, the result must match a strict sequential apply.
func TestUnpackIsDeterministicAcrossWorkerCounts(t *testing.T) {
	layers := [][]entry{
		{{name: "a", body: "1"}, {name: "d/x", body: "1"}, {name: "big", body: "b", pad: 1 << 17}},
		{{name: "a", body: "2"}, {name: "d/.wh..wh..opq"}, {name: "d", dir: true}, {name: "d/y", body: "2"}},
		{{name: "a", body: "3"}, {name: "e/z", body: "3"}},
	}
	var reference []string
	for _, workers := range []int{1, 2, 8} {
		root := unpackLayers(t, workers, layers...)
		var paths []string
		err := filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			paths = append(paths, rel)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(paths)
		if got := readFile(t, root, "a"); got != "3" {
			t.Errorf("workers=%d: a = %q, want %q", workers, got, "3")
		}
		if reference == nil {
			reference = paths
			continue
		}
		if strings.Join(paths, "\n") != strings.Join(reference, "\n") {
			t.Errorf("workers=%d produced a different tree:\n%v\nwant:\n%v", workers, paths, reference)
		}
	}
}

// The same replacement with a lower layer that is still writing under the
// directory when the higher layer's symlink arrives. The layers unpack
// concurrently, so the symlink may land before, during or after the lower
// layer's files; every order must end with the symlink and no error (a lower
// entry whose parent was pulled away used to fail with "no such file" or
// "file exists", or leave the directory in place).
func TestUnpackSymlinkReplacingDirectoryWinsWhileLowerLayerIsWriting(t *testing.T) {
	var lower []entry
	lower = append(lower, entry{name: "lib", dir: true})
	for i := 0; i < 64; i++ {
		lower = append(lower, entry{name: fmt.Sprintf("lib/sub%d/file%d.so", i%8, i), body: "old"})
	}
	for i := 0; i < 200; i++ {
		root := unpackLayers(t, 4, lower, []entry{{name: "lib", symlink: "usr/lib"}})
		info, err := os.Lstat(filepath.Join(root, "lib"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("round %d: lib mode = %v, want the symlink of the higher layer", i, info.Mode())
		}
	}
}
