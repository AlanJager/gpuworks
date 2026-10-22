package distribution

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const archivePath = "releases/abc1234/gpuworks-abc1234-linux-amd64.tar.gz"

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return data
}

// twoSources publishes the same index and archive on two stores and returns
// the index as fetched, with first as the source that won.
func twoSources(t *testing.T, data []byte) (*objectStore, *objectStore, *Client, *Fetched, File) {
	t.Helper()
	first, second := newObjectStore(t), newObjectStore(t)
	file := blob(archivePath, data)
	for _, s := range []*objectStore{first, second} {
		s.put(archivePath, data)
	}
	publish(t, testIndex("nightly", ReleaseFile{OS: "linux", Arch: "amd64", File: file}), first, second)
	client := clientFor(first, second)
	fetched, err := client.ProbeIndex(context.Background(), "nightly")
	if err != nil {
		t.Fatal(err)
	}
	// Pin the order so each test knows which source is tried first.
	fetched.Source = first.URL()
	return first, second, client, fetched, file
}

func TestDownloadFromTheWinningSource(t *testing.T) {
	data := randomBytes(t, 300<<10)
	first, second, client, fetched, file := twoSources(t, data)
	dest := filepath.Join(t.TempDir(), "archive.tar.gz")
	var last Progress
	source, err := client.Download(context.Background(), fetched, file, dest, func(p Progress) { last = p })
	if err != nil {
		t.Fatal(err)
	}
	if source != first.URL() {
		t.Fatalf("source = %s", source)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ")
	}
	if last.Done != int64(len(data)) || last.Total != int64(len(data)) || last.Source != first.URL() {
		t.Fatalf("last progress = %+v", last)
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatal("the .part file must be renamed away")
	}
	for _, request := range second.requestLog() {
		if strings.HasPrefix(request, archivePath) {
			t.Fatalf("the second source should not have been asked: %v", second.requestLog())
		}
	}
}

func TestDownloadFallsBackWhenTheFileIsMissing(t *testing.T) {
	data := randomBytes(t, 4096)
	first, second, client, fetched, file := twoSources(t, data)
	first.fail(archivePath, fault{status: 404})
	dest := filepath.Join(t.TempDir(), "a")
	source, err := client.Download(context.Background(), fetched, file, dest, progressNone)
	if err != nil || source != second.URL() {
		t.Fatalf("Download = %s, %v", source, err)
	}
}

// A connection reset halfway resumes on the next source from the byte where
// it stopped, instead of starting over.
func TestDownloadResumesOnAnotherSourceAfterACut(t *testing.T) {
	data := randomBytes(t, 512<<10)
	first, second, client, fetched, file := twoSources(t, data)
	first.fail(archivePath, fault{cutAfter: 200 << 10})
	dest := filepath.Join(t.TempDir(), "a")
	source, err := client.Download(context.Background(), fetched, file, dest, progressNone)
	if err != nil || source != second.URL() {
		t.Fatalf("Download = %s, %v", source, err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, data) {
		t.Fatal("resumed file differs")
	}
	var ranged string
	for _, request := range second.requestLog() {
		if strings.HasPrefix(request, archivePath) {
			ranged = request
		}
	}
	if !strings.HasPrefix(ranged, archivePath+" bytes=") || ranged == archivePath+" bytes=0-" {
		t.Fatalf("second source should get a range request from the cut point, got %q", ranged)
	}
}

// A transfer that stops sending is abandoned after the stall timeout, however
// long the whole download would otherwise take.
func TestDownloadAbandonsAStalledTransfer(t *testing.T) {
	data := randomBytes(t, 256<<10)
	first, second, client, fetched, file := twoSources(t, data)
	first.fail(archivePath, fault{stallAfter: 64 << 10})
	dest := filepath.Join(t.TempDir(), "a")
	start := time.Now()
	source, err := client.Download(context.Background(), fetched, file, dest, progressNone)
	if err != nil || source != second.URL() {
		t.Fatalf("Download = %s, %v", source, err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("stalled transfer held the download for %s", elapsed)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, data) {
		t.Fatal("file differs after a stall")
	}
}

// A mirror that ignores Range sends the whole file to a writer that already
// holds a prefix; the partial copy is dropped and the next try starts clean.
func TestDownloadRestartsWhenARangeIsIgnored(t *testing.T) {
	data := randomBytes(t, 128<<10)
	first, second, client, fetched, file := twoSources(t, data)
	first.fail(archivePath, fault{cutAfter: 32 << 10, once: true})
	second.fail(archivePath, fault{ignoreRange: true})
	dest := filepath.Join(t.TempDir(), "a")
	if _, err := client.Download(context.Background(), fetched, file, dest, progressNone); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, data) {
		t.Fatal("file differs after an ignored range")
	}
}

func TestDownloadDiscardsContentThatDoesNotMatch(t *testing.T) {
	data := randomBytes(t, 8192)
	first, second, client, fetched, file := twoSources(t, data)
	corrupt := append([]byte(nil), data...)
	corrupt[100] ^= 0xff
	first.put(archivePath, corrupt)
	dest := filepath.Join(t.TempDir(), "a")
	source, err := client.Download(context.Background(), fetched, file, dest, progressNone)
	if err != nil || source != second.URL() {
		t.Fatalf("Download = %s, %v", source, err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, data) {
		t.Fatal("corrupt bytes leaked into the result")
	}
}

// A mirror caught halfway through a sync serves an object of another length.
// That is wrong content, not a cut to resume: the partial copy is dropped and
// the other source supplies the file.
func TestDownloadTreatsAWrongLengthAsWrongContent(t *testing.T) {
	data := randomBytes(t, 4096)
	first, second, client, fetched, file := twoSources(t, data)
	first.put(archivePath, data[:1000])
	dest := filepath.Join(t.TempDir(), "a")
	source, err := client.Download(context.Background(), fetched, file, dest, progressNone)
	if err != nil || source != second.URL() {
		t.Fatalf("Download = %s, %v", source, err)
	}
	for _, request := range second.requestLog() {
		if strings.HasPrefix(request, archivePath+" bytes=") {
			t.Fatalf("a wrong-length object must not be resumed elsewhere: %v", second.requestLog())
		}
	}
}

// Resuming past the end of a shorter object gets a 416, which also means the
// object is not the indexed file.
func TestDownloadDropsThePartialCopyOnRangeNotSatisfiable(t *testing.T) {
	data := randomBytes(t, 4096)
	first, second, client, fetched, file := twoSources(t, data)
	second.put(archivePath, data[:100])
	dest := filepath.Join(t.TempDir(), "a")
	if err := os.WriteFile(dest+".part", data[:2000], 0o600); err != nil {
		t.Fatal(err)
	}
	first.fail(archivePath, fault{status: 503})
	_, err := client.Download(context.Background(), fetched, file, dest, progressNone)
	if err == nil || !strings.Contains(err.Error(), "object ends before byte 2000") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(dest + ".part"); !os.IsNotExist(statErr) {
		t.Fatal("the partial copy must be dropped after a 416")
	}
}

func TestDownloadFailsWhenEverySourceFails(t *testing.T) {
	data := randomBytes(t, 1024)
	first, second, client, fetched, file := twoSources(t, data)
	first.fail(archivePath, fault{status: 403})
	second.put(archivePath, randomBytes(t, 1024))
	dest := filepath.Join(t.TempDir(), "a")
	_, err := client.Download(context.Background(), fetched, file, dest, progressNone)
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err = %v, want both sources' reasons", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("nothing may be left at dest")
	}
	if _, statErr := os.Stat(dest + ".part"); !os.IsNotExist(statErr) {
		t.Fatal("a mismatching partial file must be removed")
	}
}

func TestDownloadUsesACompletePartialFile(t *testing.T) {
	data := randomBytes(t, 2048)
	first, _, client, fetched, file := twoSources(t, data)
	dest := filepath.Join(t.TempDir(), "a")
	if err := os.WriteFile(dest+".part", data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Download(context.Background(), fetched, file, dest, progressNone); err != nil {
		t.Fatal(err)
	}
	for _, request := range first.requestLog() {
		if strings.HasPrefix(request, archivePath) {
			t.Fatalf("a complete partial file needs no request: %v", first.requestLog())
		}
	}
}

func TestDownloadStopsOnCancellation(t *testing.T) {
	data := randomBytes(t, 64<<10)
	first, _, client, fetched, file := twoSources(t, data)
	first.fail(archivePath, fault{delay: 5 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := client.Download(ctx, fetched, file, filepath.Join(t.TempDir(), "a"), progressNone)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestReadFileFallsBackOnBadContent(t *testing.T) {
	data := []byte(`{"schema_version":3}`)
	first, second, client, fetched, _ := twoSources(t, randomBytes(t, 10))
	file := blob("catalog/images/x.json", data)
	first.put(file.Path, []byte(`{"schema_version":4}`))
	second.put(file.Path, data)
	got, source, err := client.ReadFile(context.Background(), fetched, file)
	if err != nil || source != second.URL() || !bytes.Equal(got, data) {
		t.Fatalf("ReadFile = %q, %s, %v", got, source, err)
	}
	if _, _, err := client.ReadFile(context.Background(), fetched, File{Path: "../x", Size: 1, SHA256: goodSHA}); err == nil {
		t.Fatal("an unsafe path must be refused")
	}
}

func TestFileOrder(t *testing.T) {
	fetched := &Fetched{Source: "b", Attempts: []Attempt{
		{Source: "a", Err: errors.New("down")},
		{Source: "b", Latency: 50 * time.Millisecond},
		{Source: "c", Latency: 300 * time.Millisecond},
		{Source: "d", Latency: 100 * time.Millisecond},
		{Source: "e", Err: ErrNotAwaited},
	}}
	if got := strings.Join(fetched.fileOrder(), ","); got != "b,d,c,a,e" {
		t.Fatalf("fileOrder = %s", got)
	}
}

func TestContentRangeStart(t *testing.T) {
	for header, want := range map[string]int64{"bytes 100-199/200": 100, " bytes 0-9/10": 0} {
		if got, ok := contentRangeStart(header); !ok || got != want {
			t.Errorf("contentRangeStart(%q) = %d, %v", header, got, ok)
		}
	}
	for _, bad := range []string{"", "items 1-2/3", "bytes x-1/2", "bytes 12"} {
		if _, ok := contentRangeStart(bad); ok {
			t.Errorf("contentRangeStart(%q) should fail", bad)
		}
	}
}
