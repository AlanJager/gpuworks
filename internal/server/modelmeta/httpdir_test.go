package modelmeta

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// directoryFixture is a faithful Apache mod_autoindex file server: the listing
// page prints sizes rounded for people, while HEAD, GET, Content-Length,
// Accept-Ranges and 206 all report the real bytes. The gap between the two is
// the whole reason this source needs its own preview path.
type directoryFixture struct {
	root  string
	files map[string]directoryFile

	requests []string
}

type directoryFile struct {
	body       string
	listedSize string
}

func newDirectoryFixture(t *testing.T, root string, files map[string]directoryFile) (*directoryFixture, string) {
	t.Helper()
	fixture := &directoryFixture{root: root, files: files}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	return fixture, server.URL + root
}

func (f *directoryFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
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
		_, _ = io.WriteString(w, f.index(r.URL.Path))
		return
	}
	file, ok := f.files[rel]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, rel, time.Unix(1788000000, 0).UTC(), strings.NewReader(file.body))
}

func (f *directoryFixture) index(urlPath string) string {
	var out strings.Builder
	out.WriteString("<html>\n<head><title>Index of " + urlPath + "</title></head>\n<body>\n<table>\n")
	out.WriteString(`<tr><th><a href="?C=N;O=D">Name</a></th><th><a href="?C=S;O=A">Size</a></th></tr>` + "\n")
	out.WriteString(`<tr><td><a href="/">Parent Directory</a></td><td align="right">  - </td></tr>` + "\n")
	for _, name := range slices.Sorted(maps.Keys(f.files)) {
		out.WriteString(`<tr><td><a href="` + name + `">` + name + `</a></td><td align="right">2026-09-01 14:43  </td><td align="right">` +
			f.files[name].listedSize + `  </td></tr>` + "\n")
	}
	out.WriteString("</table>\n<address>Apache/2.4.6 (CentOS)</address>\n</body></html>\n")
	return out.String()
}

func (f *directoryFixture) headCount() int {
	count := 0
	for _, entry := range f.requests {
		if strings.HasPrefix(entry, "HEAD ") {
			count++
		}
	}
	return count
}

const qwenConfig = `{"architectures":["Qwen3ForCausalLM"],"model_type":"qwen3","hidden_size":1024,` +
	`"num_hidden_layers":28,"max_position_embeddings":40960,"torch_dtype":"bfloat16"}`

func singleFileRepository() map[string]directoryFile {
	return map[string]directoryFile{
		"config.json":           {body: qwenConfig, listedSize: "726"},
		"tokenizer_config.json": {body: `{"model_type":"qwen3"}`, listedSize: "22"},
		"model.safetensors":     {body: strings.Repeat("w", 9973), listedSize: "9.7K"},
	}
}

func TestPreviewDirectoryReadsARepositoryFromAListing(t *testing.T) {
	files := singleFileRepository()
	fixture, endpoint := newDirectoryFixture(t, "/Qwen/Qwen3-0.6B", files)

	preview, err := PreviewDirectory(context.Background(), endpoint+"/?C=N;O=D")
	if err != nil {
		t.Fatal(err)
	}
	if preview.SourceURL != endpoint {
		t.Fatalf("source_url = %q, want the normalized %q", preview.SourceURL, endpoint)
	}
	if preview.SourceID != "Qwen/Qwen3-0.6B" || preview.Name != "Qwen3-0.6B" {
		t.Fatalf("identity = %q / %q", preview.SourceID, preview.Name)
	}
	if preview.Arch != "Qwen3ForCausalLM" || preview.Precision != "bfloat16" || preview.ContextLength != 40960 {
		t.Fatalf("model facts = %+v", preview)
	}
	if preview.FilesCount != len(files) {
		t.Fatalf("files_count = %d, want %d", preview.FilesCount, len(files))
	}
	if !slices.Equal(preview.InputModalities, []string{"text"}) {
		t.Fatalf("input_modalities = %v", preview.InputModalities)
	}
	// The listing rounds 9973 bytes to "9.7K" (9932). A single-file model is
	// worth one HEAD, because its weight size is the whole VRAM estimate.
	if preview.WeightBytes != int64(len(files["model.safetensors"].body)) {
		t.Fatalf("weight_bytes = %d, want the exact %d", preview.WeightBytes, len(files["model.safetensors"].body))
	}
	if heads := fixture.headCount(); heads != 1 {
		t.Fatalf("preview issued %d HEAD requests, want exactly one: %v", heads, fixture.requests)
	}
}

func TestPreviewDirectoryPrefersTheShardIndexOverRoundedSizes(t *testing.T) {
	files := map[string]directoryFile{
		"config.json":                      {body: qwenConfig, listedSize: "726"},
		"model-00001-of-00002.safetensors": {body: strings.Repeat("a", 4096), listedSize: "4.0K"},
		"model-00002-of-00002.safetensors": {body: strings.Repeat("b", 4096), listedSize: "4.0K"},
		"model.safetensors.index.json":     {body: `{"metadata":{"total_size":8589934592},"weight_map":{}}`, listedSize: "53"},
	}
	fixture, endpoint := newDirectoryFixture(t, "/Qwen/Qwen3-235B", files)

	preview, err := PreviewDirectory(context.Background(), endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if preview.WeightBytes != 8589934592 {
		t.Fatalf("weight_bytes = %d, want the index total 8589934592", preview.WeightBytes)
	}
	// A sharded repository must not cost one request per shard.
	if heads := fixture.headCount(); heads != 0 {
		t.Fatalf("a sharded preview issued %d HEAD requests: %v", heads, fixture.requests)
	}
}

func TestPreviewDirectoryFallsBackFromAFileURLToItsDirectory(t *testing.T) {
	files := singleFileRepository()
	_, endpoint := newDirectoryFixture(t, "/Qwen/Qwen3-0.6B", files)

	preview, err := PreviewDirectory(context.Background(), endpoint+"/model.safetensors")
	if err != nil {
		t.Fatal(err)
	}
	if preview.SourceURL != endpoint || preview.SourceID != "Qwen/Qwen3-0.6B" {
		t.Fatalf("a pasted file URL registered %q / %q", preview.SourceURL, preview.SourceID)
	}
}

// A repository whose name merely looks like a file name must not be mistaken
// for one: the HEAD answers with a directory index, so the URL stands.
func TestPreviewDirectoryKeepsADirectoryWhoseNameLooksLikeAFile(t *testing.T) {
	_, endpoint := newDirectoryFixture(t, "/Qwen/Qwen3-0.6B-v1.5", singleFileRepository())

	preview, err := PreviewDirectory(context.Background(), endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if preview.SourceURL != endpoint || preview.Name != "Qwen3-0.6B-v1.5" {
		t.Fatalf("preview = %q / %q", preview.SourceURL, preview.Name)
	}
}

// A directory needs no config.json to be a model: which files it holds is the
// engine's business. Without one the preview states no metadata, and a GGUF-only
// directory is weighed by its GGUF files.
func TestPreviewDirectoryAcceptsADirectoryWithoutConfig(t *testing.T) {
	_, gguf := newDirectoryFixture(t, "/gguf", map[string]directoryFile{
		"model-00001-of-00002.gguf": {body: "ab", listedSize: "2"},
		"model-00002-of-00002.gguf": {body: "cde", listedSize: "3"},
		"README.md":                 {body: "r", listedSize: "1"},
	})
	preview, err := PreviewDirectory(context.Background(), gguf)
	if err != nil {
		t.Fatal(err)
	}
	if preview.ConfigRaw != "" || preview.Arch != "" || preview.ContextLength != 0 || preview.FilesCount != 3 || preview.WeightBytes != 5 {
		t.Fatalf("preview = %+v", preview)
	}
}

func TestPreviewDirectoryRefusesWhatIsNotAModel(t *testing.T) {
	_, empty := newDirectoryFixture(t, "/empty", map[string]directoryFile{})
	_, malformed := newDirectoryFixture(t, "/broken", map[string]directoryFile{
		"config.json": {body: "{not json", listedSize: "9"},
	})
	for _, tc := range []struct {
		name, url string
		want      error
	}{
		{name: "no file at all", url: empty, want: ErrDirectoryNotAModel},
		{name: "unparseable config.json", url: malformed, want: ErrDirectoryNotAModel},
		{name: "not a URL", url: "not a url at all", want: ErrDirectoryURLInvalid},
		{name: "wrong scheme", url: "ftp://mirror.example.com/models", want: ErrDirectoryURLInvalid},
		{name: "embedded credentials", url: "https://user:secret@mirror.example.com/models", want: ErrDirectoryURLInvalid},
		{name: "host that does not answer", url: "http://127.0.0.1:1/models", want: ErrDirectoryUnreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preview, err := PreviewDirectory(context.Background(), tc.url)
			if preview != nil || !errors.Is(err, tc.want) {
				t.Fatalf("preview = %+v, err = %v, want %v", preview, err, tc.want)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("directory error exposed credentials: %v", err)
			}
		})
	}
}

func TestPreviewDirectoryRefusesATreeLargerThanARepository(t *testing.T) {
	files := map[string]directoryFile{"config.json": {body: qwenConfig, listedSize: "726"}}
	for index := 0; index < 5001; index++ {
		files[fmt.Sprintf("part-%05d.bin", index)] = directoryFile{body: "x", listedSize: "1"}
	}
	_, endpoint := newDirectoryFixture(t, "/huge", files)

	_, err := PreviewDirectory(context.Background(), endpoint)
	if !errors.Is(err, ErrDirectoryTooLarge) {
		t.Fatalf("err = %v, want ErrDirectoryTooLarge", err)
	}
}

// A redirect that leaves the registered host must not be followed, or the
// preview would describe one server and the download another.
func TestPreviewDirectoryStaysOnTheHostItWasGiven(t *testing.T) {
	_, real := newDirectoryFixture(t, "/Qwen/Qwen3-0.6B", singleFileRepository())
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, real+"/", http.StatusFound)
	}))
	defer redirector.Close()

	_, err := PreviewDirectoryWithClient(context.Background(), redirector.URL+"/Qwen/Qwen3-0.6B", redirector.Client())
	if !errors.Is(err, ErrDirectoryUnreachable) {
		t.Fatalf("err = %v, want the off-host redirect refused", err)
	}
}

func TestFetchForADirectorySkipsTheRepositoryRecordAndKeepsTheRest(t *testing.T) {
	files := singleFileRepository()
	files["README.md"] = directoryFile{body: "# Qwen3-0.6B\n", listedSize: "13"}
	fixture, endpoint := newDirectoryFixture(t, "/Qwen/Qwen3-0.6B", files)

	meta, err := Fetch(context.Background(), SourceURL, "Qwen/Qwen3-0.6B", "", "", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Arch != "Qwen3ForCausalLM" || meta.ContextLength != 40960 || meta.Precision != "bfloat16" {
		t.Fatalf("meta = %+v", meta)
	}
	if !strings.Contains(meta.README, "Qwen3-0.6B") {
		t.Fatalf("readme = %q", meta.README)
	}
	// A plain file server has no repository record, so there is nothing to
	// fetch and nothing to fail: no origin timestamps, no logo, and no request
	// to a repository API that does not exist.
	if meta.OriginCreatedAt != nil || meta.OriginUpdatedAt != nil || meta.LogoURL != "" {
		t.Fatalf("a directory produced repository metadata: %+v", meta)
	}
	if meta.ResolvedRevision != "" {
		t.Fatalf("resolved revision = %q, want none", meta.ResolvedRevision)
	}
	for _, entry := range fixture.requests {
		if strings.Contains(entry, "/api/models") || strings.Contains(entry, "/api/v1/models") {
			t.Fatalf("a repository API was contacted: %v", fixture.requests)
		}
	}
	// Without a shard index the single weight file is HEADed once for an exact
	// size, rather than left at the listing's rounded "9.7K" (9932 bytes).
	if meta.WeightBytes != int64(len(files["model.safetensors"].body)) {
		t.Fatalf("weight_bytes = %d, want %d", meta.WeightBytes, len(files["model.safetensors"].body))
	}
}

func TestFetchForADirectoryRefusesAnEndpointItCannotConfine(t *testing.T) {
	if _, err := Fetch(context.Background(), SourceURL, "Qwen/Qwen3-0.6B", "", "", ""); err == nil {
		t.Fatal("a url model with no directory was fetched anyway")
	}
}

func TestSourceClientOnlyConfinesDirectories(t *testing.T) {
	client, err := sourceClient(modelrepo.SourceHF, "https://huggingface.co", nil)
	if err != nil || client != nil {
		t.Fatalf("huggingface client = %v, err = %v; want the process default", client, err)
	}
	directory, err := sourceClient(SourceURL, "https://mirror.example.com/Qwen/Qwen3-0.6B", nil)
	if err != nil || directory == nil || directory.CheckRedirect == nil {
		t.Fatalf("directory client = %v, err = %v", directory, err)
	}
}

func TestLooksLikeFileNameKeepsVersionedRepositoryNames(t *testing.T) {
	for _, tc := range []struct {
		segment string
		want    bool
	}{
		{segment: "model.safetensors", want: true},
		{segment: "config.json", want: true},
		{segment: "model.bin", want: true},
		{segment: "weights.gguf", want: true},
		{segment: "Qwen3-0.6B"},
		{segment: "Llama-3.3-70B-Instruct"},
		{segment: "Qwen3-0.6B-v1.5"},
		{segment: "Qwen3-0.6B."},
		{segment: "mistral-7b-v0.3"},
		{segment: ""},
	} {
		if got := looksLikeFileName(tc.segment); got != tc.want {
			t.Errorf("looksLikeFileName(%q) = %v, want %v", tc.segment, got, tc.want)
		}
	}
}
