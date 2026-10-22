package catalogasset

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tarEntry struct {
	name     string
	typeflag byte
	data     string
}

func tarGzipFixture(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var result bytes.Buffer
	gz := gzip.NewWriter(&result)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Typeflag: entry.typeflag, Mode: 0o777, Size: int64(len(entry.data))}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if entry.data != "" {
			if _, err := tw.Write([]byte(entry.data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

func TestExtractTarGzipUsesCanonicalFilesAndAtomicDestination(t *testing.T) {
	data := tarGzipFixture(t,
		tarEntry{name: "runtime/", typeflag: tar.TypeDir},
		tarEntry{name: "runtime/serve.py", typeflag: tar.TypeReg, data: "print('ok')\n"},
	)
	destination := filepath.Join(t.TempDir(), "sha256")
	if err := ExtractTarGzip(bytes.NewReader(data), destination); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(destination, "runtime", "serve.py"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	if err := ExtractTarGzip(bytes.NewReader(data), destination); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("cache overwrite error=%v", err)
	}
}

func TestTarGzipRejectsTraversalLinksBinariesDuplicatesAndBombs(t *testing.T) {
	tests := []struct {
		name    string
		entries []tarEntry
		want    string
	}{
		{name: "traversal", entries: []tarEntry{{name: "../escape.py", typeflag: tar.TypeReg}}, want: "unsafe"},
		{name: "directory traversal", entries: []tarEntry{{name: "../escape/", typeflag: tar.TypeDir}}, want: "unsafe"},
		{name: "absolute", entries: []tarEntry{{name: "/escape.py", typeflag: tar.TypeReg}}, want: "unsafe"},
		{name: "symlink", entries: []tarEntry{{name: "link.py", typeflag: tar.TypeSymlink}}, want: "forbidden entry"},
		{name: "hardlink", entries: []tarEntry{{name: "link.py", typeflag: tar.TypeLink}}, want: "forbidden entry"},
		{name: "binary", entries: []tarEntry{{name: "lib.so", typeflag: tar.TypeReg}}, want: "forbidden file"},
		{name: "duplicate", entries: []tarEntry{{name: "a.py", typeflag: tar.TypeReg}, {name: "a.py", typeflag: tar.TypeReg}}, want: "duplicate"},
		{name: "large file", entries: []tarEntry{{name: "a.py", typeflag: tar.TypeReg, data: strings.Repeat("x", int(MaxFileBytes)+1)}}, want: "exceeds"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := tarGzipFixture(t, test.entries...)
			if _, err := ValidateTarGzip(bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}
