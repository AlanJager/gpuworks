package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// autoindexFile is one file a fixture file server holds: the bytes it really
// serves, and the size text its directory index prints for them. The two
// differ on purpose — a mod_autoindex size column carries two significant
// digits, so the listing never states the file's real length.
type autoindexFile struct {
	body       string
	listedSize string
}

// autoindexFixture is a faithful plain HTTP file server: a mod_autoindex
// directory page, HEAD, Content-Length, Accept-Ranges and 206 responses.
//
// The behaviors a download depends on are switchable, because the agent has to
// survive all of them: a server that ignores Range (the download falls back to
// one connection and cannot resume), and a server that closes the connection
// part way through a body.
type autoindexFixture struct {
	root  string
	files map[string]autoindexFile
	// listedOnly names files the index advertises but the server does not
	// serve, which is what a mirror looks like mid-resynchronization.
	listedOnly map[string]string

	// noRange serves the whole body and advertises no range support.
	noRange bool

	// breakFile names the file whose body is cut short; breakAfter is where.
	// The break is consumed by the request that trips it, so the retry that
	// follows meets a healthy server and has to resume rather than restart.
	breakFile  string
	breakAfter int

	mu       sync.Mutex
	broken   bool
	requests []string
	ranges   []string
}

func newAutoindexFixture(t *testing.T, root string, files map[string]autoindexFile) (*autoindexFixture, string, *httptest.Server) {
	t.Helper()
	fixture := &autoindexFixture{root: root, files: files}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	return fixture, server.URL + root, server
}

func (f *autoindexFixture) record(entry string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, entry)
}

func (f *autoindexFixture) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *autoindexFixture) rangeHeaders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ranges...)
}

// takeBreak reports where this file's body should be cut short, once.
func (f *autoindexFixture) takeBreak(rel string, length int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken || f.breakFile != rel || f.breakAfter <= 0 || f.breakAfter >= length {
		return 0
	}
	f.broken = true
	return f.breakAfter
}

func (f *autoindexFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.record(r.Method + " " + r.URL.Path)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasPrefix(r.URL.Path, f.root) {
		http.NotFound(w, r)
		return
	}
	rel := strings.Trim(strings.TrimPrefix(r.URL.Path, f.root), "/")
	if rel == "" {
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		w.Header().Set("Content-Type", "text/html;charset=UTF-8")
		_, _ = w.Write([]byte(f.index(r.URL.Path)))
		return
	}
	file, ok := f.files[rel]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if f.noRange {
		w.Header().Set("Content-Length", fmt.Sprint(len(file.body)))
		w.Header().Set("Content-Type", "application/octet-stream")
		if r.Method == http.MethodHead {
			return
		}
		_, _ = io.WriteString(w, file.body)
		return
	}
	if header := r.Header.Get("Range"); header != "" {
		f.mu.Lock()
		f.ranges = append(f.ranges, header)
		f.mu.Unlock()
	}
	if at := f.takeBreak(rel, len(file.body)); at > 0 && r.Method == http.MethodGet {
		// A body that stops early without a Content-Length promise is the
		// shape a dropped connection really has on the wire.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", fmt.Sprint(len(file.body)))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, file.body[:at])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		panic(http.ErrAbortHandler)
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", `"`+rel+`"`)
	http.ServeContent(w, r, rel, time.Unix(1788000000, 0).UTC(), strings.NewReader(file.body))
}

// index renders mod_autoindex's FancyIndexing table, sort links, parent row
// and rounded size column included.
func (f *autoindexFixture) index(urlPath string) string {
	var out strings.Builder
	out.WriteString("<html>\n<head><title>Index of " + urlPath + "</title></head>\n<body>\n<table>\n")
	out.WriteString(`<tr><th><a href="?C=N;O=D">Name</a></th><th><a href="?C=S;O=A">Size</a></th></tr>` + "\n")
	out.WriteString(`<tr><td><a href="/">Parent Directory</a></td><td align="right">  - </td></tr>` + "\n")
	rows := map[string]string{}
	for name, file := range f.files {
		rows[name] = file.listedSize
	}
	for name, size := range f.listedOnly {
		rows[name] = size
	}
	for _, name := range slices.Sorted(maps.Keys(rows)) {
		out.WriteString(`<tr><td><a href="` + name + `">` + name + `</a></td><td align="right">2026-09-01 14:43  </td><td align="right">` +
			rows[name] + `  </td></tr>` + "\n")
	}
	out.WriteString("</table>\n<address>Apache/2.4.6 (CentOS)</address>\n</body></html>\n")
	return out.String()
}

// urlModelFiles is a single-shard repository whose listed sizes are rounded
// below the real bodies, which is the case every correction has to handle.
func urlModelFiles() map[string]autoindexFile {
	return map[string]autoindexFile{
		"config.json":       {body: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"]}`, listedSize: "58"},
		"model.safetensors": {body: strings.Repeat("w", 3000), listedSize: "2.9K"},
	}
}

func urlModelRuntime(t *testing.T, root string) (*NodeRuntime, *[]proto.ModelFileStatus) {
	t.Helper()
	runtime := &NodeRuntime{log: slog.New(slog.NewTextHandler(io.Discard, nil)), modelsDir: root, downloading: map[string]*modelDownload{}}
	var mu sync.Mutex
	reports := []proto.ModelFileStatus{}
	runtime.Bind(func(kind string, value any) error {
		if kind == proto.TStatusModelFile {
			mu.Lock()
			reports = append(reports, value.(proto.ModelFileStatus))
			mu.Unlock()
		}
		return nil
	})
	return runtime, &reports
}

func urlTask(endpoint string) proto.ModelFileTask {
	return proto.ModelFileTask{
		Key: "31-4", Source: modelrepo.SourceURL, SourceID: "Qwen/Qwen3-0.6B", Revision: "",
		Endpoint: endpoint, TargetPath: "model", DesiredState: "present",
		Acquisition: proto.ModelAcquireDownload,
	}
}

func TestDownloadRepoFromDirectoryConvergesOnTheExactSize(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "model")
	files := urlModelFiles()
	_, endpoint, _ := newAutoindexFixture(t, "/Qwen/Qwen3-0.6B", files)
	runtime, reports := urlModelRuntime(t, root)
	task := urlTask(endpoint)

	runtime.downloadRepo(context.Background(), task, target, target)

	exact := int64(len(files["config.json"].body) + len(files["model.safetensors"].body))
	final := (*reports)[len(*reports)-1]
	if final.State != proto.MFPresent {
		t.Fatalf("final status = %+v", final)
	}
	// The listing said 58 + 2.9K = 3028; the real repository is 3058 bytes, so
	// a total that was never corrected would leave progress above 100%.
	if final.BytesTotal != exact || final.BytesDone != exact {
		t.Fatalf("final total=%d done=%d, want %d", final.BytesTotal, final.BytesDone, exact)
	}
	if final.BytesTotal == 3028 {
		t.Fatal("the listing's rounded total was reported as the repository size")
	}
	for _, report := range *reports {
		if report.BytesDone > report.BytesTotal {
			t.Fatalf("reported %d of %d bytes: progress exceeded 100%%", report.BytesDone, report.BytesTotal)
		}
	}
}

func TestDownloadRepoFromDirectoryRecordsMeasuredSizesInTheMarker(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "model")
	files := urlModelFiles()
	fixture, endpoint, _ := newAutoindexFixture(t, "/Qwen/Qwen3-0.6B", files)
	runtime, _ := urlModelRuntime(t, root)
	task := urlTask(endpoint)

	runtime.downloadRepo(context.Background(), task, target, target)

	marker, ok := readCompletionMarker(target)
	if !ok {
		t.Fatal("no completion marker was written")
	}
	for _, file := range marker.Files {
		want := int64(len(files[file.Path].body))
		if file.Size != want {
			t.Errorf("marker records %s as %d bytes, want the measured %d", file.Path, file.Size, want)
		}
	}
	// A marker holding measured sizes is what lets the unchanged, exact marker
	// check accept this repository.
	size, valid := validCompletionMarker(target, task)
	if !valid || size != marker.Bytes {
		t.Fatalf("marker check = size %d valid %v, marker bytes %d", size, valid, marker.Bytes)
	}

	before := fixture.requestCount()
	runtime.reconcileModelFile(context.Background(), task)
	if after := fixture.requestCount(); after != before {
		t.Fatalf("second reconcile issued %d more requests; the marker should have short-circuited it", after-before)
	}
}

func TestDownloadRepoFromDirectoryResumesAfterAnInterruptedBody(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "model")
	files := urlModelFiles()
	fixture, endpoint, _ := newAutoindexFixture(t, "/Qwen/Qwen3-0.6B", files)
	fixture.breakFile, fixture.breakAfter = "model.safetensors", 900
	runtime, reports := urlModelRuntime(t, root)

	runtime.downloadRepo(context.Background(), urlTask(endpoint), target, target)

	final := (*reports)[len(*reports)-1]
	if final.State != proto.MFPresent {
		t.Fatalf("an interrupted body was not recovered: %+v", final)
	}
	body, err := os.ReadFile(filepath.Join(target, "model.safetensors"))
	if err != nil || string(body) != files["model.safetensors"].body {
		t.Fatalf("resumed file is %d bytes (err %v), want %d", len(body), err, len(files["model.safetensors"].body))
	}
	resumed := false
	for _, header := range fixture.rangeHeaders() {
		if header != "" && !strings.HasPrefix(header, "bytes=0-") {
			resumed = true
		}
	}
	if !resumed {
		t.Fatalf("no request resumed from a non-zero offset: %v", fixture.rangeHeaders())
	}
}

func TestDownloadRepoFromDirectoryWithoutRangeSupportStillCompletes(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "model")
	files := urlModelFiles()
	fixture, endpoint, _ := newAutoindexFixture(t, "/Qwen/Qwen3-0.6B", files)
	fixture.noRange = true
	runtime, reports := urlModelRuntime(t, root)

	runtime.downloadRepo(context.Background(), urlTask(endpoint), target, target)

	final := (*reports)[len(*reports)-1]
	exact := int64(len(files["config.json"].body) + len(files["model.safetensors"].body))
	if final.State != proto.MFPresent || final.BytesTotal != exact {
		t.Fatalf("status against a server without range support = %+v, want %d bytes present", final, exact)
	}
}

func TestDownloadRepoFromDirectoryReportsAVanishedFileWithItsURL(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "model")
	files := urlModelFiles()
	fixture, endpoint, _ := newAutoindexFixture(t, "/Qwen/Qwen3-0.6B", map[string]autoindexFile{
		"config.json": files["config.json"],
	})
	// The index still advertises the shard, but the server no longer serves
	// it — the ordinary shape of a mirror re-synchronized mid-download.
	fixture.listedOnly = map[string]string{"model.safetensors": "2.9K"}
	runtime, reports := urlModelRuntime(t, root)

	runtime.downloadRepo(context.Background(), urlTask(endpoint), target, target)

	final := (*reports)[len(*reports)-1]
	if final.State != proto.MFFailed {
		t.Fatalf("missing shard produced %+v", final)
	}
	if !strings.Contains(final.Message, endpoint+"/model.safetensors") {
		t.Fatalf("failure message %q does not name the file's URL", final.Message)
	}
}

func TestDownloadRepoFromDirectoryRefusesWhenTheDiskCannotHoldIt(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "model")
	// A listing that claims a petabyte cannot fit on any test machine, so the
	// refusal is the disk admission and not a transport failure.
	_, endpoint, _ := newAutoindexFixture(t, "/Qwen/Qwen3-0.6B", map[string]autoindexFile{
		"config.json":       {body: `{"model_type":"qwen3"}`, listedSize: "22"},
		"model.safetensors": {body: "w", listedSize: "900P"},
	})
	runtime, reports := urlModelRuntime(t, root)

	runtime.downloadRepo(context.Background(), urlTask(endpoint), target, target)

	final := (*reports)[len(*reports)-1]
	if final.State != proto.MFInsufficientDisk {
		t.Fatalf("status = %+v, want insufficient disk space", final)
	}
	if _, err := os.Stat(filepath.Join(target, "model.safetensors")); !os.IsNotExist(err) {
		t.Fatalf("a refused download still wrote the shard: %v", err)
	}
}

func TestAdmissionBytesBudgetsForRoundedListingSizes(t *testing.T) {
	const tenGiB = int64(10) << 30
	for _, tc := range []struct {
		name        string
		remaining   int64
		approximate bool
		want        int64
	}{
		{name: "exact listing is budgeted as stated", remaining: tenGiB, want: tenGiB},
		{name: "rounded listing carries the extra share", remaining: tenGiB, approximate: true, want: tenGiB + tenGiB/16},
		{name: "nothing left to download needs no budget", remaining: 0, approximate: true},
		{name: "an overflowing budget falls back to the stated size", remaining: math.MaxInt64 - 8, approximate: true, want: math.MaxInt64 - 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := admissionBytes(tc.remaining, tc.approximate); got != tc.want {
				t.Fatalf("admissionBytes(%d, %v) = %d, want %d", tc.remaining, tc.approximate, got, tc.want)
			}
		})
	}
}

func TestModelSourceClientConfinesADirectoryToItsHost(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "weights from somewhere else")
	}))
	defer elsewhere.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/model.safetensors", http.StatusFound)
	}))
	defer redirector.Close()

	client, err := modelSourceClient(modelrepo.SourceURL, redirector.URL+"/Qwen/Qwen3-0.6B", proto.NetworkConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(redirector.URL + "/Qwen/Qwen3-0.6B/model.safetensors"); err == nil {
		t.Fatal("a redirect off the registered host was followed")
	} else if !strings.Contains(err.Error(), "redirected from host") {
		t.Fatalf("error = %v, want a host refusal", err)
	}

	// Another source keeps the plain client: it has no single host to be
	// confined to.
	plain, err := modelSourceClient(modelrepo.SourceHF, "", proto.NetworkConfig{})
	if err != nil || plain != nil {
		t.Fatalf("client for huggingface = %v, err %v; want the downloader's default", plain, err)
	}
}

func TestSourceURLRewriteLeavesADirectoryURLAlone(t *testing.T) {
	net := proto.NetworkConfig{Rewrites: []proto.URLRewrite{{From: "https://huggingface.co", To: "https://mirror.example.com"}}}
	if rewrite := sourceURLRewrite(modelrepo.SourceURL, net); rewrite != nil {
		t.Fatal("a directory URL was offered a mirror rewrite")
	}
	rewrite := sourceURLRewrite(modelrepo.SourceHF, net)
	if rewrite == nil || rewrite("https://huggingface.co/a/b") != "https://mirror.example.com/a/b" {
		t.Fatal("the mirror rewrite no longer applies to huggingface")
	}
}

// A repository API states exact sizes, so a transfer that produced different
// bytes is a corrupt download and must still be refused. Only a listing that
// could not state the size is corrected from what landed on disk.
func TestDownloadRepoStillRefusesARepositoryFileOfTheWrongSize(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "model")
	config := `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"]}`
	weights := strings.Repeat("w", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/org/model/tree/main":
			// The listing promises 128 bytes; the file server delivers 64.
			_, _ = fmt.Fprintf(w, `[{"type":"file","path":"config.json","size":%d},{"type":"file","path":"model.safetensors","size":128}]`, len(config))
		case "/org/model/resolve/main/config.json":
			_, _ = io.WriteString(w, config)
		case "/org/model/resolve/main/model.safetensors":
			_, _ = io.WriteString(w, weights)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	runtime, reports := urlModelRuntime(t, root)

	runtime.downloadRepo(context.Background(), proto.ModelFileTask{
		Key: "9-1", Source: modelrepo.SourceHF, SourceID: "org/model", Revision: "main",
		Endpoint: server.URL, TargetPath: "model", DesiredState: "present", Acquisition: proto.ModelAcquireDownload,
	}, target, target)

	final := (*reports)[len(*reports)-1]
	if final.State != proto.MFFailed || !strings.Contains(final.Message, "download verification failed") {
		t.Fatalf("status = %+v, want the short file refused", final)
	}
	if _, ok := readCompletionMarker(target); ok {
		t.Fatal("a refused download still wrote a completion marker")
	}
}
