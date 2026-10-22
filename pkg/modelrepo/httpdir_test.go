package modelrepo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestListHTTPDirReadsAnApacheAutoindexDirectory(t *testing.T) {
	fixture := newFixture(t, flavorApache, "/Qwen/Qwen3-0.6B")
	fixture.dirs[""] = qwen3Entries()
	files, err := listFixture(t, fixture)
	if err != nil {
		t.Fatal(err)
	}
	got := fileMap(files)
	want := map[string]int64{
		"config.json":            726,
		"configuration.json":     73,
		"generation_config.json": 239,
		"merges.txt":             1677721,
		"model.safetensors":      1503238553,
		"tokenizer.json":         11534336,
		"tokenizer_config.json":  9728,
		"vocab.json":             2726297,
	}
	if len(got) != len(want) {
		t.Fatalf("listed %d files: %+v", len(got), files)
	}
	for path, size := range want {
		if got[path] != size {
			t.Errorf("%s size = %d, want %d", path, got[path], size)
		}
	}
	// A listing is returned in a stable order so two registrations of the same
	// directory describe the same repository.
	for index := 1; index < len(files); index++ {
		if files[index-1].Path >= files[index].Path {
			t.Fatalf("listing is not sorted: %q then %q", files[index-1].Path, files[index].Path)
		}
	}
	// The sort links, the parent-directory row and the address footer must not
	// have produced entries of their own.
	for _, file := range files {
		if strings.Contains(file.Path, "?") || strings.Contains(file.Path, "Parent") {
			t.Fatalf("chrome row became a file: %q", file.Path)
		}
		if file.Revision != "" {
			t.Fatalf("%s carries revision %q; a directory index has none", file.Path, file.Revision)
		}
	}
}

func TestListHTTPDirReadsAShardedRepositoryOfOneHundredAndEighteenParts(t *testing.T) {
	fixture := newFixture(t, flavorApache, "/Qwen/Qwen3-235B-A22B-Instruct-2507")
	entries := []indexEntry{{name: "config.json", size: "1.2K"}, {name: "config_1m.json", size: "1.3K"}}
	for shard := 1; shard <= 118; shard++ {
		entries = append(entries, indexEntry{name: fmt.Sprintf("model-%05d-of-00118.safetensors", shard), size: "4.0G"})
	}
	fixture.dirs[""] = entries
	files, err := listFixture(t, fixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 120 {
		t.Fatalf("listed %d files, want 120", len(files))
	}
	got := fileMap(files)
	if got["model-00001-of-00118.safetensors"] != 4<<30 || got["model-00118-of-00118.safetensors"] != 4<<30 {
		t.Fatalf("shard sizes = %d/%d", got["model-00001-of-00118.safetensors"], got["model-00118-of-00118.safetensors"])
	}
	if got["config.json"] != 1228 {
		t.Fatalf("config.json size = %d, want 1228", got["config.json"])
	}
	// One request per directory: a per-file HEAD would be 118 extra requests
	// for sizes the index already carries.
	if requests := fixture.requests.Load(); requests != 1 {
		t.Fatalf("listing issued %d requests, want 1", requests)
	}
}

func TestListHTTPDirReadsNginxAndCaddyIndexes(t *testing.T) {
	t.Run("nginx", func(t *testing.T) {
		fixture := newFixture(t, flavorNginx, "/models/Qwen3-0.6B")
		fixture.dirs[""] = []indexEntry{
			{name: "config.json", size: "726"},
			{name: "model.safetensors", size: "1503300328"},
			{name: "extra", isDir: true},
		}
		fixture.dirs["extra"] = []indexEntry{{name: "note.txt", size: "12"}}
		files, err := listFixture(t, fixture)
		if err != nil {
			t.Fatal(err)
		}
		got := fileMap(files)
		if got["config.json"] != 726 || got["model.safetensors"] != 1503300328 || got["extra/note.txt"] != 12 {
			t.Fatalf("nginx listing = %+v", got)
		}
		if len(got) != 3 {
			t.Fatalf("nginx listing has %d files: %+v", len(got), got)
		}
	})
	t.Run("caddy", func(t *testing.T) {
		fixture := newFixture(t, flavorCaddy, "/files/Qwen3-0.6B")
		fixture.dirs[""] = []indexEntry{
			{name: "config.json", size: "726 B"},
			{name: "model.safetensors", size: "1.4 GiB"},
			{name: "tokenizer_config.json", size: "9.5 KiB"},
			{name: "nested", isDir: true},
		}
		fixture.dirs["nested"] = []indexEntry{{name: "note.txt", size: "12 B"}}
		files, err := listFixture(t, fixture)
		if err != nil {
			t.Fatal(err)
		}
		got := fileMap(files)
		want := map[string]int64{
			"config.json":           726,
			"model.safetensors":     1503238553,
			"tokenizer_config.json": 9728,
			"nested/note.txt":       12,
		}
		for path, size := range want {
			if got[path] != size {
				t.Errorf("%s size = %d, want %d", path, got[path], size)
			}
		}
		if len(got) != len(want) {
			t.Fatalf("caddy listing = %+v", got)
		}
	})
}

func TestListHTTPDirWalksNestedDirectoriesAndRefusesToGoDeeper(t *testing.T) {
	// depth 1 and depth 3 are inside the budget; the first directory found
	// below depth 3 must abort rather than silently truncate the repository.
	for _, tc := range []struct {
		name    string
		depth   int
		wantErr string
	}{
		{name: "depth 1", depth: 1},
		{name: "depth 3", depth: 3},
		{name: "depth 4", depth: 4, wantErr: "maximum depth of 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newFixture(t, flavorApache, "/repo/model")
			prefix := ""
			for level := 1; level <= tc.depth; level++ {
				parent := strings.TrimPrefix(prefix+"/", "/")
				parent = strings.TrimSuffix(parent, "/")
				fixture.dirs[parent] = []indexEntry{
					{name: fmt.Sprintf("file%d.bin", level), size: "1.0K"},
					{name: fmt.Sprintf("level%d", level), isDir: true},
				}
				if prefix == "" {
					prefix = fmt.Sprintf("level%d", level)
				} else {
					prefix += fmt.Sprintf("/level%d", level)
				}
			}
			fixture.dirs[prefix] = []indexEntry{{name: "leaf.bin", size: "2.0K"}}
			files, err := listFixture(t, fixture)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				if files != nil {
					t.Fatalf("a refused listing still returned %d files", len(files))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := fileMap(files)
			if len(got) != tc.depth+1 {
				t.Fatalf("listed %+v", got)
			}
			leaf := prefix + "/leaf.bin"
			if got[leaf] != 2048 {
				t.Fatalf("deepest file %q = %d, want 2048", leaf, got[leaf])
			}
		})
	}
}

func TestListHTTPDirRejectsListingsItCannotVouchFor(t *testing.T) {
	t.Run("empty directory", func(t *testing.T) {
		fixture := newFixture(t, flavorApache, "/repo/empty")
		fixture.dirs[""] = nil
		_, err := listFixture(t, fixture)
		if err == nil || !strings.Contains(err.Error(), "returned no files") {
			t.Fatalf("error = %v, want no-files refusal", err)
		}
	})
	t.Run("non-HTML response", func(t *testing.T) {
		fixture := newFixture(t, flavorApache, "/repo/bucket")
		fixture.raw[""] = rawResponse{contentType: "application/xml",
			body: `<?xml version="1.0"?><ListBucketResult><Contents><Key>config.json</Key></Contents></ListBucketResult>`}
		_, err := listFixture(t, fixture)
		if err == nil || !strings.Contains(err.Error(), "not an HTML directory index") {
			t.Fatalf("error = %v, want content-type refusal", err)
		}
	})
	t.Run("missing directory", func(t *testing.T) {
		fixture := newFixture(t, flavorApache, "/repo/model")
		fixture.dirs["present"] = []indexEntry{{name: "a.bin", size: "1"}}
		_, err := listFixture(t, fixture)
		if err == nil || !strings.Contains(err.Error(), "http 404") {
			t.Fatalf("error = %v, want 404", err)
		}
	})
	t.Run("entry limit exceeded", func(t *testing.T) {
		fixture := newFixture(t, flavorApache, "/repo/huge")
		entries := make([]indexEntry, 0, 5001)
		for index := 0; index < 5001; index++ {
			entries = append(entries, indexEntry{name: fmt.Sprintf("part-%05d.bin", index), size: "1.0K"})
		}
		fixture.dirs[""] = entries
		_, err := listFixture(t, fixture)
		if err == nil || !strings.Contains(err.Error(), "maximum of 5000 entries") {
			t.Fatalf("error = %v, want entry-limit refusal", err)
		}
	})
	t.Run("body limit exceeded", func(t *testing.T) {
		fixture := newFixture(t, flavorApache, "/repo/fat")
		var body strings.Builder
		body.WriteString("<html><body><table>\n")
		for body.Len() <= 8<<20 {
			body.WriteString(`<tr><td><a href="pad.bin">pad.bin</a></td><td align="right">1.0K</td></tr>` + "\n")
		}
		body.WriteString("</table></body></html>")
		fixture.raw[""] = rawResponse{contentType: "text/html", body: body.String()}
		_, err := listFixture(t, fixture)
		if err == nil || !errors.Is(err, ErrListingTooLarge) {
			t.Fatalf("error = %v, want body-limit refusal", err)
		}
	})
}

// Every href shape that could take the walker outside the directory it was
// pointed at must be dropped, and dropping them must not cost the legitimate
// entry sharing the row.
func TestListHTTPDirRejectsHostileHrefs(t *testing.T) {
	hostile := []string{
		"../../etc/passwd",
		"/absolute",
		"https://evil/x",
		"%2e%2e%2ffoo",
		"a/../../b",
		"?C=N;O=D",
		"../",
		"a/./b",
		"..%2fescape",
		"https://evil.example/Qwen/Qwen3-0.6B/model.safetensors",
		"//evil.example/x",
	}
	fixture := newFixture(t, flavorApache, "/Qwen/Qwen3-0.6B")
	entries := []indexEntry{{name: "config.json", size: "726"}}
	for index, href := range hostile {
		entries = append(entries, indexEntry{name: fmt.Sprintf("bad%d", index), size: "1.0K", rawHref: href})
	}
	fixture.dirs[""] = entries
	files, err := listFixture(t, fixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "config.json" {
		t.Fatalf("hostile hrefs survived: %+v", files)
	}
	if requests := fixture.requests.Load(); requests != 1 {
		t.Fatalf("a hostile href was fetched: %d requests", requests)
	}
}

// An off-site href that happens to sit under the same path prefix on another
// host, and a same-host href that climbs out of the listed directory, are both
// outside the repository even though they look plausible.
func TestListHTTPDirAcceptsOnlyChildrenOfTheListedDirectory(t *testing.T) {
	fixture := newFixture(t, flavorApache, "/Qwen/Qwen3-0.6B")
	fixture.dirs[""] = []indexEntry{
		{name: "config.json", size: "726"},
		{name: "sibling", size: "1.0K", rawHref: "../Qwen3-1.7B/model.safetensors"},
		{name: "deep", size: "1.0K", rawHref: "sub/deeper/file.bin"},
	}
	fixture.dirs["sub"] = []indexEntry{{name: "deeper", isDir: true}}
	files, err := listFixture(t, fixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "config.json" {
		t.Fatalf("listing = %+v", files)
	}
}

// The fixture server must behave like the real one in the ways the rest of the
// import path depends on: HEAD works, ranges are honoured, a missing path is a
// 404 and a directory without its trailing slash redirects.
func TestHTTPDirFixtureServesLikeARealFileServer(t *testing.T) {
	fixture := newFixture(t, flavorApache, "/Qwen/Qwen3-0.6B")
	fixture.dirs[""] = qwen3Entries()
	fixture.files["config.json"] = `{"architectures":["Qwen3ForCausalLM"]}`
	server, base := fixture.start(t)
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	head, err := client.Head(base + "/config.json")
	if err != nil {
		t.Fatal(err)
	}
	defer head.Body.Close()
	if head.StatusCode != http.StatusOK || head.ContentLength != int64(len(fixture.files["config.json"])) {
		t.Fatalf("HEAD status=%d length=%d", head.StatusCode, head.ContentLength)
	}
	if head.Header.Get("Accept-Ranges") != "bytes" || head.Header.Get("ETag") == "" {
		t.Fatalf("HEAD headers = %v", head.Header)
	}

	request, err := http.NewRequest(http.MethodGet, base+"/config.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Range", "bytes=0-4")
	partial, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer partial.Body.Close()
	if partial.StatusCode != http.StatusPartialContent {
		t.Fatalf("range status = %d", partial.StatusCode)
	}

	index, err := client.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer index.Body.Close()
	if contentType := index.Header.Get("Content-Type"); !strings.Contains(contentType, "text/html") {
		t.Fatalf("index content-type = %q", contentType)
	}

	redirect, err := client.Get(base)
	if err != nil {
		t.Fatal(err)
	}
	defer redirect.Body.Close()
	if redirect.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("directory without trailing slash = %d", redirect.StatusCode)
	}

	missing, err := client.Get(base + "/does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("missing path = %d", missing.StatusCode)
	}
}

// The node's proxy and URL acceleration settings reach the listing through the
// same RequestOptions the download path uses; a listing that ignored them
// would fail on exactly the nodes that need them.
func TestListHTTPDirHonorsInjectedClientAndRewrite(t *testing.T) {
	fixture := newFixture(t, flavorApache, "/mirror/Qwen3-0.6B")
	fixture.dirs[""] = []indexEntry{{name: "config.json", size: "726"}}
	server, base := fixture.start(t)
	rewritten := make(chan string, 4)
	files, err := ListFiles(context.Background(), SourceURL, "Qwen/Qwen3-0.6B", "", "https://huggingface.co/Qwen/Qwen3-0.6B", nil,
		RequestOptions{
			Client: server.Client(),
			Rewrite: func(raw string) string {
				rewritten <- raw
				return strings.Replace(raw, "https://huggingface.co/Qwen/Qwen3-0.6B", base, 1)
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "config.json" {
		t.Fatalf("listing = %+v", files)
	}
	select {
	case got := <-rewritten:
		if got != "https://huggingface.co/Qwen/Qwen3-0.6B/" {
			t.Fatalf("rewrite saw %q", got)
		}
	default:
		t.Fatal("rewrite was never consulted")
	}
}

func TestListHTTPDirRejectsAnUnusableDirectoryURL(t *testing.T) {
	for _, endpoint := range []string{"", "ftp://mirror/models", "https://user:pass@mirror/models"} {
		if _, err := ListFiles(context.Background(), SourceURL, "org/model", "", endpoint, nil); err == nil {
			t.Fatalf("endpoint %q was accepted", endpoint)
		}
	}
}

// An unknown source used to be listed as Hugging Face, so an agent too old to
// know a source would produce a 404 against huggingface.co instead of saying
// it cannot serve the request.
func TestListFilesRefusesAnUnsupportedSource(t *testing.T) {
	_, err := ListFiles(context.Background(), "local", "org/model", "", "https://example.invalid", nil)
	if err == nil || !strings.Contains(err.Error(), `unsupported model source "local"`) {
		t.Fatalf("error = %v, want unsupported source", err)
	}
}

func TestURLSourceResolvesFilesAgainstTheDirectoryAndHasNoRevision(t *testing.T) {
	if got := ResolveURL(SourceURL, "Qwen/Qwen3-0.6B", "", "model.safetensors", "https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B"); got != "https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B/model.safetensors" {
		t.Fatalf("ResolveURL = %q", got)
	}
	if got := ResolveURL(SourceURL, "Qwen/Qwen3-0.6B", "", "sub dir/weights v2+#.bin", "https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B"); got != "https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B/sub%20dir/weights%20v2+%23.bin" {
		t.Fatalf("decoded path must be re-escaped per segment, got %q", got)
	}
	if got := ResolveURL(SourceURL, "Qwen/Qwen3-0.6B", "ignored", "sub/shard.bin", "https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B/"); got != "https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B/sub/shard.bin" {
		t.Fatalf("ResolveURL with trailing slash = %q", got)
	}
	revision, err := ResolveRevision(context.Background(), SourceURL, "Qwen/Qwen3-0.6B", "", "https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B", nil)
	if err != nil || revision != "" {
		t.Fatalf("ResolveRevision = %q, %v", revision, err)
	}
	if !SizeIsApproximate(SourceURL) {
		t.Fatal("url listing sizes must be reported as approximate")
	}
	for _, source := range []string{SourceHF, SourceMS, "local", ""} {
		if SizeIsApproximate(source) {
			t.Fatalf("%q sizes must not be reported as approximate", source)
		}
	}
}
