package modelscan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

func p(root, rel string) string { return filepath.Join(root, filepath.FromSlash(rel)) }

// hfModel writes a flat model repo: config, two shards, tokenizer.
func hfModel(t *testing.T, root, rel string) {
	t.Helper()
	write(t, p(root, rel+"/config.json"), `{"architectures":["X"]}`)
	write(t, p(root, rel+"/model-00001-of-00002.safetensors"), strings.Repeat("a", 100))
	write(t, p(root, rel+"/model-00002-of-00002.safetensors"), strings.Repeat("b", 50))
	write(t, p(root, rel+"/tokenizer.json"), "{}")
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// buildLibrary reproduces the shapes seen in the real NFS library.
func buildLibrary(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	// ModelScope download with an empty ._____temp.
	hfModel(t, root, "Qwen/Qwen3-8B")
	write(t, p(root, "Qwen/Qwen3-8B/README.md"), "# Qwen3\n")
	write(t, p(root, "Qwen/Qwen3-8B/.msc"), "binary")
	write(t, p(root, "Qwen/Qwen3-8B/.mv"), "Revision:master,CreatedAt:1756691498")
	mkdir(t, p(root, "Qwen/Qwen3-8B/._____temp"))

	// HuggingFace download state, two spellings.
	hfModel(t, root, "Qwen/Qwen3-32B")
	write(t, p(root, "Qwen/Qwen3-32B/.huggingface/download/config.json.metadata"), sha+"\n\"etag\"\n1756691498.1\n")
	hfModel(t, root, "Qwen/Qwen3-14B")
	write(t, p(root, "Qwen/Qwen3-14B/.cache/huggingface/download/config.json.metadata"), "main\n\"etag\"\n1\n")
	write(t, p(root, "Qwen/Qwen3-14B/.cache/huggingface/download/sub/tokenizer.json.metadata"), strings.ToUpper(sha)+"\n")

	// Unfinished HuggingFace download.
	hfModel(t, root, "Qwen/Qwen3-4B")
	write(t, p(root, "Qwen/Qwen3-4B/.huggingface/download/model-00002.safetensors.incomplete"), "partial")

	// ModelScope with a non-empty temp dir.
	hfModel(t, root, "org-a/ms-temp")
	write(t, p(root, "org-a/ms-temp/.msc"), "")
	write(t, p(root, "org-a/ms-temp/._____temp/chunk"), "x")

	// gpuworks marker.
	hfModel(t, root, "acme/marked")
	write(t, p(root, "acme/marked/"+CompletionMarker),
		`{"version":2,"source":"huggingface","source_id":"acme/Marked-7B","revision":"abc","subfolder":"fp8","files":[],"bytes":150,"created_at":"2026-01-01T00:00:00Z"}`)
	// Corrupt marker falls back to the tool metadata.
	hfModel(t, root, "acme/badmarker")
	write(t, p(root, "acme/badmarker/"+CompletionMarker), "{not json")
	write(t, p(root, "acme/badmarker/.mv"), "Revision:v1")

	// Plain directory with a stray .part file, and a three-level path.
	hfModel(t, root, "local/plain")
	write(t, p(root, "local/plain/model.safetensors.part"), "p")
	hfModel(t, root, "team/org/model")

	// Mistral native layout, no config.json.
	writeMistral(t, p(root, "mistralai/Mistral-native"))

	// Skips.
	write(t, p(root, "deepseek-ai/DeepSeek-V3.1/config.json"), "")
	write(t, p(root, "deepseek-ai/DeepSeek-V3.1/model-00001.safetensors"), "")
	mkdir(t, p(root, "deepseek-ai/Empty"))
	write(t, p(root, "Comfy-Org/Repack/README.md"), "readme")
	write(t, p(root, "Comfy-Org/Repack/.gitattributes"), "*")
	write(t, p(root, "Comfy-Org/Repack/.huggingface/download/x.metadata"), sha)
	write(t, p(root, "Comfy-Org/Licensed/LICENSE"), "mit")
	write(t, p(root, "misc/onlyjson/tokenizer.json"), "{}")
	write(t, p(root, "misc/configonly/config.json"), "{}")
	mkdir(t, p(root, "hfcache/models--a--b/blobs"))
	mkdir(t, p(root, "hfcache/models--a--b/snapshots/abc"))
	write(t, p(root, "hfcache/models--a--b/blobs/deadbeef"), "w")
	mkdir(t, p(root, "d1/d2/d3/d4/d5"))
	write(t, p(root, "d1/d2/d3/d4/d5/config.json"), "{}")
	write(t, p(root, "d1/d2/d3/d4/d5/m.safetensors"), "w")

	// A non-model directory with a stray config.json is still walked.
	write(t, p(root, "weird/config.json"), "{}")
	write(t, p(root, "weird/docs/guide.md"), "doc")

	// Never entered or reported.
	hfModel(t, root, ".git/hidden")
	hfModel(t, root, "Qwen/.trash/model")

	return root
}

func byRel(res Result) map[string]Candidate {
	m := map[string]Candidate{}
	for _, c := range res.Candidates {
		m[c.RelPath] = c
	}
	return m
}

func skippedMap(res Result) map[string]string {
	m := map[string]string{}
	for _, s := range res.Skipped {
		m[s.RelPath] = s.Reason
	}
	return m
}

func TestScanFindsModelsAndIdentities(t *testing.T) {
	root := buildLibrary(t)
	res, err := Scan(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Complete || res.NextCursor != "" {
		t.Fatalf("complete=%v cursor=%q", res.Complete, res.NextCursor)
	}
	got := byRel(res)

	check := func(rel, basis, source, id, rev string) Candidate {
		t.Helper()
		c, ok := got[rel]
		if !ok {
			t.Fatalf("%s not a candidate; have %v", rel, res.Candidates)
		}
		if c.Basis != basis || c.Source != source || c.SourceID != id || c.Revision != rev {
			t.Fatalf("%s identity = %+v", rel, c)
		}
		return c
	}

	ms := check("Qwen/Qwen3-8B", BasisModelScope, SourceModelScope, "Qwen/Qwen3-8B", "master")
	// 100+50 shard bytes + 2 config bytes... compute from the files actually written.
	if want := int64(len(`{"architectures":["X"]}`) + 150 + 2 + len("# Qwen3\n")); ms.SizeBytes != want {
		t.Fatalf("size=%d want %d (dot files must not count)", ms.SizeBytes, want)
	}
	if ms.Files != 5 || ms.Incomplete {
		t.Fatalf("files=%d incomplete=%v reason=%q", ms.Files, ms.Incomplete, ms.IncompleteReason)
	}

	check("Qwen/Qwen3-32B", BasisHuggingFace, SourceHuggingFace, "Qwen/Qwen3-32B", sha)
	// Revision "main" is not a sha; the next metadata file supplies one.
	check("Qwen/Qwen3-14B", BasisHuggingFace, SourceHuggingFace, "Qwen/Qwen3-14B", strings.ToUpper(sha))
	c := check("Qwen/Qwen3-4B", BasisHuggingFace, SourceHuggingFace, "Qwen/Qwen3-4B", "")
	if !c.Incomplete || c.IncompleteReason != "*.incomplete present" {
		t.Fatalf("incomplete=%v reason=%q", c.Incomplete, c.IncompleteReason)
	}
	c = check("org-a/ms-temp", BasisModelScope, SourceModelScope, "org-a/ms-temp", "")
	if !c.Incomplete || c.IncompleteReason != "non-empty ._____temp" {
		t.Fatalf("incomplete=%v reason=%q", c.Incomplete, c.IncompleteReason)
	}
	c = check("acme/marked", BasisMarker, SourceHuggingFace, "acme/Marked-7B", "abc")
	if c.Subfolder != "fp8" {
		t.Fatalf("subfolder=%q", c.Subfolder)
	}
	check("acme/badmarker", BasisModelScope, SourceModelScope, "acme/badmarker", "v1")
	c = check("local/plain", BasisPath, SourceLocal, "local/plain", "")
	if !c.Incomplete || c.IncompleteReason != "*.part present" {
		t.Fatalf("incomplete=%v reason=%q", c.Incomplete, c.IncompleteReason)
	}
	check("team/org/model", BasisPath, SourceLocal, "team/org/model", "")
	check("mistralai/Mistral-native", BasisPath, SourceLocal, "mistralai/Mistral-native", "")

	for rel := range got {
		if strings.Contains(rel, ".git") || strings.Contains(rel, ".trash") {
			t.Fatalf("dot directory leaked: %s", rel)
		}
	}
	// Models are never entered.
	if _, ok := got["Qwen/Qwen3-8B/._____temp"]; ok {
		t.Fatal("entered a model directory")
	}

	skipped := skippedMap(res)
	want := map[string]string{
		"deepseek-ai/DeepSeek-V3.1": SkipEmpty,
		"deepseek-ai/Empty":         SkipEmpty,
		"Comfy-Org/Repack":          SkipMetadataOnly,
		"Comfy-Org/Licensed":        SkipMetadataOnly,
		"misc/onlyjson":             SkipUnrecognizedLayout,
		"misc/configonly":           SkipUnrecognizedLayout,
		"hfcache/models--a--b":      SkipHFHubCache,
		"weird/docs":                SkipMetadataOnly,
		"d1/d2/d3/d4":               SkipTooDeep,
	}
	for rel, reason := range want {
		if skipped[rel] != reason {
			t.Errorf("skipped[%s]=%q want %q (all: %v)", rel, skipped[rel], reason, res.Skipped)
		}
	}
	// Dot directories and ordinary parents are not reported; the hub cache is
	// not entered.
	for rel := range skipped {
		if _, ok := want[rel]; !ok {
			t.Errorf("unexpected skip %s (%s)", rel, skipped[rel])
		}
	}
}

func TestScanOrderIsBreadthFirstAndSorted(t *testing.T) {
	root := t.TempDir()
	hfModel(t, root, "b/m")
	hfModel(t, root, "a/z/deep")
	hfModel(t, root, "a/m")
	hfModel(t, root, "top")
	res, err := Scan(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range res.Candidates {
		got = append(got, c.RelPath)
	}
	want := []string{"top", "a/m", "b/m", "a/z/deep"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order=%v want %v", got, want)
	}
}

func TestScanPaginationIsExactAndStable(t *testing.T) {
	root := buildLibrary(t)
	full, err := Scan(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{1, 2, 3, 5, len(full.Candidates), len(full.Candidates) + 1} {
		var cands []string
		skipped := map[string]int{}
		cursor := ""
		for page := 0; ; page++ {
			if page > 100 {
				t.Fatalf("limit %d: did not terminate", limit)
			}
			res, err := Scan(context.Background(), root, Options{Limit: limit, Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Candidates) > limit {
				t.Fatalf("limit %d: page has %d", limit, len(res.Candidates))
			}
			for _, c := range res.Candidates {
				cands = append(cands, c.RelPath)
			}
			for _, s := range res.Skipped {
				skipped[s.RelPath]++
			}
			if res.Complete {
				if res.NextCursor != "" {
					t.Fatalf("complete page has cursor %q", res.NextCursor)
				}
				break
			}
			if res.NextCursor != res.Candidates[len(res.Candidates)-1].RelPath {
				t.Fatalf("cursor=%q", res.NextCursor)
			}
			cursor = res.NextCursor
		}
		var want []string
		for _, c := range full.Candidates {
			want = append(want, c.RelPath)
		}
		if strings.Join(cands, ",") != strings.Join(want, ",") {
			t.Fatalf("limit %d: candidates %v want %v", limit, cands, want)
		}
		for _, s := range full.Skipped {
			// A skip between two pages' candidates is reported exactly once;
			// one after the final candidate lands on the last page.
			if skipped[s.RelPath] != 1 {
				t.Fatalf("limit %d: skip %s reported %d times", limit, s.RelPath, skipped[s.RelPath])
			}
		}
		if len(skipped) != len(full.Skipped) {
			t.Fatalf("limit %d: %d distinct skips, want %d", limit, len(skipped), len(full.Skipped))
		}
	}
}

func TestScanMaxDepth(t *testing.T) {
	root := t.TempDir()
	hfModel(t, root, "a/b/c")
	res, err := Scan(context.Background(), root, Options{MaxDepth: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 0 || skippedMap(res)["a/b"] != SkipTooDeep {
		t.Fatalf("depth 2: %+v", res)
	}
	res, err = Scan(context.Background(), root, Options{MaxDepth: 3})
	if err != nil || len(res.Candidates) != 1 {
		t.Fatalf("depth 3: %+v err=%v", res, err)
	}
}

func TestScanDeadlineAndCancel(t *testing.T) {
	root := buildLibrary(t)

	res, err := Scan(context.Background(), root, Options{Deadline: time.Now().Add(-time.Second)})
	if err != nil || res.Complete || len(res.Candidates) != 0 || res.NextCursor != "" {
		t.Fatalf("deadline: %+v err=%v", res, err)
	}
	// Resuming keeps the caller's cursor when nothing was processed.
	res, err = Scan(context.Background(), root, Options{Cursor: "Qwen/Qwen3-8B", Deadline: time.Now().Add(-time.Second)})
	if err != nil || res.Complete || res.NextCursor != "Qwen/Qwen3-8B" {
		t.Fatalf("deadline with cursor: %+v err=%v", res, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err = Scan(ctx, root, Options{})
	if err != nil || res.Complete {
		t.Fatalf("cancel: %+v err=%v", res, err)
	}
}

func TestScanSkipsAfterTheLastCandidateBelongToTheNextPage(t *testing.T) {
	root := t.TempDir()
	hfModel(t, root, "a/m")
	mkdir(t, p(root, "b/empty"))
	res, err := Scan(context.Background(), root, Options{Limit: 1})
	if err != nil || len(res.Skipped) != 0 || res.NextCursor != "a/m" || res.Complete {
		t.Fatalf("first page: %+v err=%v", res, err)
	}
	res, err = Scan(context.Background(), root, Options{Cursor: "a/m"})
	if err != nil || !res.Complete || skippedMap(res)["b/empty"] != SkipEmpty || len(res.Candidates) != 0 {
		t.Fatalf("second page: %+v err=%v", res, err)
	}
}

func TestScanRootErrors(t *testing.T) {
	if _, err := Scan(context.Background(), filepath.Join(t.TempDir(), "missing"), Options{}); err == nil {
		t.Fatal("missing root must fail")
	}
	file := filepath.Join(t.TempDir(), "f")
	write(t, file, "x")
	if _, err := Scan(context.Background(), file, Options{}); err == nil {
		t.Fatal("file root must fail")
	}
}

func TestScanEmptyRootIsComplete(t *testing.T) {
	res, err := Scan(context.Background(), t.TempDir(), Options{})
	if err != nil || !res.Complete || len(res.Candidates)+len(res.Skipped) != 0 {
		t.Fatalf("%+v err=%v", res, err)
	}
}

func TestScanUnreadableDirectoryDoesNotFailScan(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
	root := t.TempDir()
	hfModel(t, root, "org/ok")
	hfModel(t, root, "org/locked")
	hfModel(t, root, "org/locked2")
	if err := os.Chmod(p(root, "org/locked"), 0o000); err != nil {
		t.Fatal(err)
	}
	// Readable directory whose content cannot be walked.
	if err := os.Chmod(p(root, "org/locked2/sub"), 0o000); err != nil {
		// no sub dir yet; create one
		mkdir(t, p(root, "org/locked2/sub"))
		write(t, p(root, "org/locked2/sub/x"), "x")
		if err := os.Chmod(p(root, "org/locked2/sub"), 0o000); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = os.Chmod(p(root, "org/locked"), 0o755)
		_ = os.Chmod(p(root, "org/locked2/sub"), 0o755)
	})
	res, err := Scan(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := byRel(res)["org/ok"]; !ok {
		t.Fatalf("readable sibling lost: %+v", res)
	}
	sk := skippedMap(res)
	if sk["org/locked"] != SkipUnreadable || sk["org/locked2"] != SkipUnreadable {
		t.Fatalf("skipped=%v", sk)
	}
	// A model whose subdirectory cannot be listed has an unknown size; it is
	// not offered with the bytes that happened to be readable.
	if c, ok := byRel(res)["org/locked2"]; ok {
		t.Fatalf("partly unreadable model offered as a candidate: %+v", c)
	}
}

func TestScanSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	hfModel(t, outside, "elsewhere")
	hfModel(t, root, "real/model")
	if err := os.Symlink(p(outside, "elsewhere"), p(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(p(root, "real"), p(root, "alias")); err != nil {
		t.Fatal(err)
	}
	// A link loop back to the root stays bounded by the depth limit.
	if err := os.Symlink(root, p(root, "real/loop")); err != nil {
		t.Fatal(err)
	}
	// A dangling link and a link to a file are ignored.
	if err := os.Symlink(p(root, "nowhere"), p(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	// A weight that is a symlink to a file counts towards the size.
	hfModel(t, root, "linked/weights")
	write(t, p(outside, "blob"), strings.Repeat("w", 40))
	if err := os.Symlink(p(outside, "blob"), p(root, "linked/weights/extra.safetensors")); err != nil {
		t.Fatal(err)
	}

	res, err := Scan(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := byRel(res)
	if _, ok := got["escape"]; ok {
		t.Fatal("followed a symlink out of the root")
	}
	if _, ok := got["alias/model"]; !ok {
		t.Fatalf("in-root symlink not followed: %v", res.Candidates)
	}
	if _, ok := got["real/model"]; !ok {
		t.Fatalf("real model missing: %v", res.Candidates)
	}
	if c := got["linked/weights"]; c.Files != 5 || c.SizeBytes < 40 {
		t.Fatalf("symlinked weight: %+v", c)
	}
}

func TestScanSymlinkedRoot(t *testing.T) {
	real := t.TempDir()
	hfModel(t, real, "org/m")
	link := filepath.Join(t.TempDir(), "lib")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	res, err := Scan(context.Background(), link, Options{})
	if err != nil || len(res.Candidates) != 1 || res.Candidates[0].RelPath != "org/m" {
		t.Fatalf("%+v err=%v", res, err)
	}
}

func TestScanIncompleteMarkers(t *testing.T) {
	root := t.TempDir()
	hfModel(t, root, "a/all")
	write(t, p(root, "a/all/x.incomplete"), "")
	write(t, p(root, "a/all/y.part"), "")
	write(t, p(root, "a/all/y.part.state"), "")
	write(t, p(root, "a/all/._____temp/z"), "")
	// Ordinary names that merely resemble the markers are fine.
	hfModel(t, root, "a/clean")
	write(t, p(root, "a/clean/partial.json"), "{}")
	mkdir(t, p(root, "a/clean/._____temp"))
	res, err := Scan(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := byRel(res)
	if r := got["a/all"].IncompleteReason; r != "*.incomplete present; *.part present; *.part.state present; non-empty ._____temp" {
		t.Fatalf("reason=%q", r)
	}
	if got["a/clean"].Incomplete {
		t.Fatalf("clean flagged: %+v", got["a/clean"])
	}
}

func TestIdentityHelpers(t *testing.T) {
	dir := t.TempDir()
	mv := filepath.Join(dir, ".mv")
	for content, want := range map[string]string{
		"Revision:master,CreatedAt:1": "master",
		"Revision: v1.0\n":            "v1.0",
		"CreatedAt:1,Revision:abc":    "abc",
		"nothing here":                "",
	} {
		write(t, mv, content)
		if got := modelScopeRevision(mv); got != want {
			t.Errorf("%q -> %q want %q", content, got, want)
		}
	}
	if modelScopeRevision(filepath.Join(dir, "absent")) != "" {
		t.Error("absent .mv must give empty revision")
	}
	if isHex40(sha[:39]) || isHex40(strings.Repeat("g", 40)) || !isHex40(sha) {
		t.Error("isHex40")
	}
	if huggingFaceRevision(filepath.Join(dir, "absent")) != "" {
		t.Error("absent download dir")
	}
}

func TestHuggingFaceRevisionProbeIsBounded(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxMetadataProbe+5; i++ {
		write(t, filepath.Join(dir, "f"+strings.Repeat("a", 3)+string(rune('a'+i%26))+string(rune('a'+i/26))+".metadata"), "main\n")
	}
	write(t, filepath.Join(dir, "zzz.metadata"), sha+"\n")
	if got := huggingFaceRevision(dir); got != "" {
		t.Fatalf("probe not bounded, got %q", got)
	}
}

func TestMarkerRequiresIdentity(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, CompletionMarker), `{"version":1,"source":"","source_id":""}`)
	if _, ok := readMarker(dir); ok {
		t.Fatal("marker without identity accepted")
	}
	if _, ok := readMarker(filepath.Join(dir, "none")); ok {
		t.Fatal("absent marker accepted")
	}
}

func TestCompareOrder(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"a", "b", -1}, {"b", "a", 1}, {"a", "a", 0},
		{"z", "a/a", -1},    // shallower first
		{"a-b/c", "a/d", 1}, // component-wise, not byte-wise ('-' < '/')
		{"a/b", "a/c", -1},
	}
	for _, c := range cases {
		got := proto.ScanOrder(c.a, c.b)
		if (got < 0) != (c.want < 0) || (got > 0) != (c.want > 0) {
			t.Errorf("proto.ScanOrder(%q,%q)=%d want sign %d", c.a, c.b, got, c.want)
		}
	}
}

// A directory whose files were fetched at different commits names none: a
// registration must not pin bytes that disagree with each other.
func TestHuggingFaceCommitNeedsOneCommitForEveryFile(t *testing.T) {
	root := t.TempDir()
	one, other := strings.Repeat("a", 40), strings.Repeat("b", 40)
	write(t, filepath.Join(root, "same/.cache/huggingface/download/config.json.metadata"), one+"\netag\n1\n")
	write(t, filepath.Join(root, "same/.cache/huggingface/download/model.safetensors.metadata"), strings.ToUpper(one)+"\netag\n1\n")
	write(t, filepath.Join(root, "mixed/.cache/huggingface/download/config.json.metadata"), one+"\netag\n1\n")
	write(t, filepath.Join(root, "mixed/.cache/huggingface/download/model.safetensors.metadata"), other+"\netag\n1\n")
	write(t, filepath.Join(root, "plain/config.json"), "{}")
	for dir, want := range map[string]string{"same": one, "mixed": "", "plain": ""} {
		if got := strings.ToLower(HuggingFaceCommit(filepath.Join(root, dir))); got != want {
			t.Errorf("%s: commit = %q, want %q", dir, got, want)
		}
	}
}
