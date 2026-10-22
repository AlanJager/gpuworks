package imagearchive

import (
	"archive/tar"
	"bytes"
	"slices"
	"testing"
)

func tail(b []byte, n int) []byte {
	if n >= len(b) {
		return b
	}
	return b[len(b)-n:]
}

func head(b []byte, n int) []byte {
	if n >= len(b) {
		return b
	}
	return b[:n]
}

func TestScanTailReadsDockerSaveRepoTags(t *testing.T) {
	want := []string{"gpustack/runner:cuda13.0-vllm0.25.1", "gpustack/runner:latest"}
	archive := dockerSaveArchive(t, dockerSaveOptions{repoTags: want, layerSize: 200_000})
	if len(archive) <= tailWindow {
		t.Fatalf("archive too small to exercise the tail window: %d bytes", len(archive))
	}
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"whole archive", archive},
		{"64KiB tail", tail(archive, tailWindow)},
		{"unaligned tail", tail(archive, tailWindow+37)},
		{"tiny 4KiB tail", tail(archive, 4096)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ScanTail(tc.data)
			if !ok {
				t.Fatalf("ScanTail returned ok=false")
			}
			if !slices.Equal(got, want) {
				t.Fatalf("tags = %q, want %q", got, want)
			}
		})
	}
}

func TestScanTailIgnoresDecoyManifestInsideLayer(t *testing.T) {
	archive := dockerSaveArchive(t, dockerSaveOptions{repoTags: []string{"real/image:v1"}, layerSize: 4096})
	got, ok := ScanTail(archive)
	if !ok {
		t.Fatalf("ScanTail returned ok=false")
	}
	if !slices.Equal(got, []string{"real/image:v1"}) {
		t.Fatalf("tags = %q, want the outer manifest's tag", got)
	}
}

func TestScanTailGNUFormat(t *testing.T) {
	archive := dockerSaveArchive(t, dockerSaveOptions{
		repoTags: []string{"gnu/image:v1"},
		format:   tar.FormatGNU,
	})
	got, ok := ScanTail(archive)
	if !ok || !slices.Equal(got, []string{"gnu/image:v1"}) {
		t.Fatalf("ScanTail = %q, %v; want [gnu/image:v1], true", got, ok)
	}
}

func TestScanTailManifestStraddlesWindow(t *testing.T) {
	tags := manyTags(1500)
	archive := dockerSaveArchive(t, dockerSaveOptions{repoTags: tags, layerSize: 8192})

	small := scanTail(tail(archive, tailWindow))
	if len(small.tags) != 0 {
		t.Fatalf("64KiB tail unexpectedly yielded %d tags", len(small.tags))
	}
	if !small.sawTar {
		t.Fatalf("64KiB tail should still look like a tar tail (repositories entry + zero blocks)")
	}
	if !small.needMore() {
		t.Fatalf("needMore() = false, want true so PeekURL retries with a bigger window")
	}

	big, ok := ScanTail(tail(archive, tailWindowLarge))
	if !ok {
		t.Fatalf("1MiB tail returned ok=false")
	}
	if !slices.Equal(big, tags) {
		t.Fatalf("1MiB tail returned %d tags, want %d", len(big), len(tags))
	}
}

func TestScanTailNoManifest(t *testing.T) {
	archive := dockerSaveArchive(t, dockerSaveOptions{omitManifest: true, layerSize: 100_000})
	res := scanTail(tail(archive, tailWindow))
	if len(res.tags) != 0 {
		t.Fatalf("tags = %q, want none", res.tags)
	}
	// The final layer payload fills the whole window, but the trailing zero
	// blocks still identify it as a tar tail, so a larger window is worth one
	// retry.
	if !res.sawTar || !res.needMore() {
		t.Fatalf("sawTar=%v needMore=%v, want true/true", res.sawTar, res.needMore())
	}

	small := dockerSaveArchive(t, dockerSaveOptions{omitManifest: true, layerSize: 4096})
	res = scanTail(small)
	if len(res.tags) != 0 || !res.sawTar {
		t.Fatalf("whole small archive: tags=%q sawTar=%v, want none/true", res.tags, res.sawTar)
	}
}

func TestScanTailGzipIsNotATar(t *testing.T) {
	archive := dockerSaveArchive(t, dockerSaveOptions{
		repoTags:  []string{"gz/image:v1"},
		layerSize: 300_000,
	})
	gz := gzipBytes(t, append(archive, incompressible(400_000)...))
	res := scanTail(tail(gz, tailWindow))
	if res.sawTar {
		t.Fatalf("sawTar = true for gzip data; a bogus header validated")
	}
	if res.needMore() {
		t.Fatalf("needMore() = true for gzip data; PeekURL would waste a 1MiB request")
	}
	if !isGzip(head(gz, 64)) {
		t.Fatalf("isGzip = false for gzip data")
	}
	if isGzip(head(archive, 64)) {
		t.Fatalf("isGzip = true for a plain tar")
	}
}

func TestScanTailRejectsCorruptChecksum(t *testing.T) {
	archive := dockerSaveArchive(t, dockerSaveOptions{repoTags: []string{"corrupt/image:v1"}, plainLayer: true})
	idx := bytes.LastIndex(archive, []byte("manifest.json\x00"))
	if idx < 0 || idx%blockSize != 0 {
		t.Fatalf("manifest.json header not found at a block boundary (idx=%d)", idx)
	}
	corrupt := slices.Clone(archive)
	// Flip a byte inside the header's size field; the checksum must catch it.
	corrupt[idx+124] ^= 0x01
	if _, ok := ScanTail(corrupt); ok {
		t.Fatalf("ScanTail accepted a header with a broken checksum")
	}
	if _, ok := ScanTail(archive); !ok {
		t.Fatalf("control: ScanTail rejected the intact archive")
	}
}

func TestScanTailTruncatedManifestContent(t *testing.T) {
	archive := dockerSaveArchive(t, dockerSaveOptions{repoTags: []string{"trunc/image:v1"}, plainLayer: true})
	idx := bytes.LastIndex(archive, []byte("manifest.json\x00"))
	if idx < 0 {
		t.Fatalf("manifest.json header not found")
	}
	// Keep the header but drop its payload and everything after it. The cut
	// stays block aligned, which is what a Range fetch of the tail guarantees.
	res := scanTail(archive[:idx+blockSize])
	if len(res.tags) != 0 {
		t.Fatalf("tags = %q, want none for a truncated manifest", res.tags)
	}
	if !res.needMore() {
		t.Fatalf("needMore() = false, want true for a truncated manifest")
	}
}

func TestScanHeadOCIIndexAnnotations(t *testing.T) {
	want := []string{"gpustack/runner:cuda13.0-sglang0.5", "gpustack/runner:latest"}
	archive := ociArchive(t, want, 8192)
	got, ok := ScanHead(head(archive, headWindow))
	if !ok {
		t.Fatalf("ScanHead returned ok=false")
	}
	if !slices.Equal(got, want) {
		t.Fatalf("tags = %q, want %q", got, want)
	}
}

func TestScanHeadSkipsLayerPayloads(t *testing.T) {
	archive := dockerSaveArchive(t, dockerSaveOptions{repoTags: []string{"real/image:v2"}, layerSize: 4096})
	got, ok := ScanHead(archive)
	if !ok {
		t.Fatalf("ScanHead returned ok=false over the whole archive")
	}
	if !slices.Equal(got, []string{"real/image:v2"}) {
		t.Fatalf("tags = %q, want the outer manifest's tag (decoy leaked?)", got)
	}
}

func TestScanHeadWindowWithoutManifest(t *testing.T) {
	archive := dockerSaveArchive(t, dockerSaveOptions{repoTags: []string{"tail/only:v1"}, layerSize: 300_000})
	if got, ok := ScanHead(head(archive, headWindow)); ok {
		t.Fatalf("ScanHead = %q, true; want false: the manifest sits at the archive tail", got)
	}
}

func TestScanHeadNoManifestAtAll(t *testing.T) {
	archive := dockerSaveArchive(t, dockerSaveOptions{omitManifest: true, layerSize: 4096})
	if got, ok := ScanHead(archive); ok {
		t.Fatalf("ScanHead = %q, true; want false", got)
	}
}

func TestParseHeaderRejectsNonHeaders(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block []byte
	}{
		{"zero block", make([]byte, blockSize)},
		{"short block", make([]byte, 100)},
		{"json payload", append([]byte(`[{"RepoTags":["a:b"]}]`), make([]byte, blockSize-22)...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := parseHeader(tc.block); ok {
				t.Fatalf("parseHeader accepted %s", tc.name)
			}
		})
	}
}

func TestParseNumeric(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field []byte
		want  int64
		ok    bool
	}{
		{"octal", []byte("0000123\x00"), 0o123, true},
		{"octal spaces", []byte(" 0000144 "), 0o144, true},
		{"empty", []byte("\x00\x00\x00\x00\x00\x00\x00\x00"), 0, true},
		{"not octal", []byte("0000098\x00"), 0, false},
		{"gnu base256", []byte{0x80, 0, 0, 0, 0, 0, 0x01, 0x00}, 256, true},
		{"gnu negative", []byte{0xc0, 0, 0, 0, 0, 0, 0, 0x01}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseNumeric(tc.field)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("parseNumeric = %d, %v; want %d, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestAppendTagsDedupesAndSkipsEmpty(t *testing.T) {
	got := appendTags(nil, "a:1", "", "  ", "a:1", " b:2 ")
	if !slices.Equal(got, []string{"a:1", "b:2"}) {
		t.Fatalf("appendTags = %q", got)
	}
}

func TestDockerRepoTagsRejectsGarbage(t *testing.T) {
	if got := dockerRepoTags([]byte("not json")); got != nil {
		t.Fatalf("dockerRepoTags = %q, want nil", got)
	}
	if got := ociIndexTags([]byte(`{"manifests":[{"annotations":{}}]}`)); got != nil {
		t.Fatalf("ociIndexTags = %q, want nil", got)
	}
}
