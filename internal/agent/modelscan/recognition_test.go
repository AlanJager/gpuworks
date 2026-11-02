package modelscan

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/model/modelmetadata"
)

func TestScanPipelineOwnershipAndPagination(t *testing.T) {
	root := t.TempDir()
	repo := "MiniMaxAI/MiniMax-H3"
	write(t, p(root, repo+"/.msc"), "modelscope")
	write(t, p(root, repo+"/README.md"), "---\npipeline_tag: image-text-to-video\n---\n")
	write(t, p(root, repo+"/modular_model_index.json"), `{"_class_name":"MiniMaxH3ModularPipeline","audio_vae":["custom","AudioVAE",{"repo":"MiniMaxAI/MiniMax-H3","subfolder":"FL2VA/audio_vae"}]}`)
	for _, sub := range []string{"FL2VA", "Ref2VA"} {
		write(t, p(root, repo+"/"+sub+"/model_index.json"), `{"_class_name":"MiniMaxH3Pipeline","audio_vae":["custom","MiniMaxH3AudioVAE"],"text_encoder":["transformers","TextEncoder"]}`)
		write(t, p(root, repo+"/"+sub+"/audio_vae/config.json"), `{"_class_name":"MiniMaxH3AudioVAE","auto_map":{"AutoModel":"audio.AudioVAE"}}`)
		write(t, p(root, repo+"/"+sub+"/audio_vae/model.safetensors"), "weights")
		hfModel(t, root, repo+"/"+sub+"/text_encoder")
	}
	full, err := Scan(context.Background(), root, Options{MaxDepth: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Candidates) != 3 {
		t.Fatalf("model entries=%+v skips=%+v", full.Candidates, full.Skipped)
	}
	for _, c := range full.Candidates {
		if strings.Contains(c.RelPath, "audio_vae") || strings.Contains(c.RelPath, "text_encoder") {
			t.Fatalf("component offered: %+v", c)
		}
		if c.RelPath == repo {
			continue
		}
		if c.SourceID != c.RelPath || c.Source != SourceLocal || c.Subfolder != "" || !slices.Equal(c.Tasks, []string{"image-text-to-video"}) {
			t.Fatalf("subentry metadata lost: %+v", c)
		}
	}
	for _, sub := range []string{"FL2VA", "Ref2VA"} {
		for _, component := range []string{"audio_vae", "text_encoder"} {
			if skippedMap(full)[repo+"/"+sub+"/"+component] != proto.ScanSkipComponent {
				t.Fatalf("component reason absent: %+v", full.Skipped)
			}
		}
	}
	var paged []Candidate
	cursor := ""
	for i := 0; i < 10; i++ {
		page, err := Scan(context.Background(), root, Options{MaxDepth: 8, Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		paged = append(paged, page.Candidates...)
		if page.Complete {
			break
		}
		if page.NextCursor == cursor {
			t.Fatal("cursor did not advance")
		}
		cursor = page.NextCursor
	}
	if !reflect.DeepEqual(paged, full.Candidates) {
		t.Fatalf("pagination lost ownership: %+v vs %+v", paged, full.Candidates)
	}
	report, err := ParseMetadataWithin(context.Background(), root, p(root, repo+"/FL2VA/audio_vae"))
	if err != nil || !report.Component || len(report.Entries) != 0 {
		t.Fatalf("import metadata parsing lost ownership: %+v %v", report, err)
	}
	report, err = ParseMetadataWithin(context.Background(), root, p(root, repo+"/FL2VA"))
	if err != nil || len(report.Entries) != 1 || !slices.Equal(report.Tasks, []string{"image-text-to-video"}) {
		t.Fatalf("import metadata parsing lost task: %+v %v", report, err)
	}
}

func TestRepositoryBoundariesAndFileView(t *testing.T) {
	root := t.TempDir()
	hfModel(t, root, "A")
	write(t, p(root, "A/README.md"), "---\npipeline_tag: text-ranking\n---\n")
	hfModel(t, root, "deep/group/B")
	if err := os.Symlink(p(root, "A/README.md"), p(root, "deep/group/B/README.md")); err != nil {
		t.Fatal(err)
	}
	view, err := OpenView(p(root, "deep/group/B"))
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	for _, name := range []string{"../A/README.md", "README.md", p(root, "A/README.md")} {
		if _, err := view.ReadRange(context.Background(), name, 0, 8); err == nil {
			t.Fatalf("scope escaped: %s", name)
		}
	}
	if _, err := view.ReadRange(context.Background(), "config.json", 0, 3<<20); !errors.Is(err, modelmetadata.ErrReadLimit) {
		t.Fatalf("read limit=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := view.ReadDir(ctx, "."); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	res, err := Scan(context.Background(), root, Options{MaxDepth: 8})
	if err != nil {
		t.Fatal(err)
	}
	b := byRel(res)["deep/group/B"]
	if len(b.Tasks) != 0 || len(b.Issues) == 0 {
		t.Fatalf("borrowed repository A task: %+v", b)
	}
	if _, err := ParseMetadataWithin(context.Background(), p(root, "A"), p(root, "deep/group/B")); !errors.Is(err, modelmetadata.ErrOutsideScope) {
		t.Fatalf("outside metadata parsing=%v", err)
	}
}

func splitFixture(arch, kind string, no, count uint16) string {
	var b strings.Builder
	u32 := func(n uint32) { var a [4]byte; binary.LittleEndian.PutUint32(a[:], n); b.Write(a[:]) }
	u64 := func(n uint64) { var a [8]byte; binary.LittleEndian.PutUint64(a[:], n); b.Write(a[:]) }
	str := func(s string) { u64(uint64(len(s))); b.WriteString(s) }
	n := uint64(0)
	if arch != "" {
		n++
	}
	if kind != "" {
		n++
	}
	if count > 0 {
		n += 2
	}
	b.WriteString("GGUF")
	u32(3)
	u64(1)
	u64(n)
	if arch != "" {
		str("general.architecture")
		u32(8)
		str(arch)
	}
	if kind != "" {
		str("general.type")
		u32(8)
		str(kind)
	}
	for _, v := range []struct {
		key   string
		value uint16
	}{{"split.no", no}, {"split.count", count}} {
		if count > 0 {
			str(v.key)
			u32(2)
			var a [2]byte
			binary.LittleEndian.PutUint16(a[:], v.value)
			b.Write(a[:])
		}
	}
	return b.String()
}

func TestScanGGUFRepositoryWithFourVariants(t *testing.T) {
	root := t.TempDir()
	repo := "alesha-pro/Strata-GGUF"
	write(t, p(root, repo+"/README.md"), "---\npipeline_tag: image-text-to-text\nlibrary_name: gguf\n---\n")
	write(t, p(root, repo+"/.cache/huggingface/download/README.md.metadata"), sha+"\netag\n1")
	write(t, p(root, repo+"/mmproj.gguf"), splitFixture("clip", "mmproj", 0, 0))
	write(t, p(root, repo+"/direction.gguf"), splitFixture("controlvector", "", 0, 0))
	for _, variant := range []string{"IQ2_XS", "IQ3_S", "IQ3_XXS", "Q2_0"} {
		write(t, p(root, repo+"/"+variant+"/model-00001-of-00002.gguf"), splitFixture("qwen4exp", "model", 0, 2))
		write(t, p(root, repo+"/"+variant+"/model-00002-of-00002.gguf"), splitFixture("", "", 1, 2))
	}
	res, err := Scan(context.Background(), root, Options{})
	if err != nil || len(res.Candidates) != 4 {
		t.Fatalf("GGUF scan=%+v err=%v", res, err)
	}
	for _, c := range res.Candidates {
		if c.SourceID != c.RelPath || c.Source != SourceLocal || c.Subfolder != "" || c.Incomplete || c.Format != "gguf" || !slices.Equal(c.Tasks, []string{"image-text-to-text"}) {
			t.Fatalf("GGUF identity=%+v", c)
		}
	}
	// Two independent single files must never be silently combined into one model.
	write(t, p(root, "mixed/a.gguf"), splitFixture("llama", "model", 0, 0))
	write(t, p(root, "mixed/b.gguf"), splitFixture("llama", "model", 0, 0))
	res, err = Scan(context.Background(), root, Options{})
	if err != nil || skippedMap(res)["mixed"] != proto.ScanSkipMultipleEntries {
		t.Fatalf("ambiguous entries=%+v %v", res, err)
	}
}

// TestIdentityRecognizesEverySourceMarker pins the shared vocabulary:
// modelmetadata.SourceMarkers owns the names, identity.go recognizes a
// ModelScope download by them, and internal/agent/app/modelcopy.go skips them
// when it copies. Each name must be sufficient on its own, so a copy that skips
// one of them can never leave behind a directory this scan would no longer
// call a ModelScope repository.
func TestIdentityRecognizesEverySourceMarker(t *testing.T) {
	for _, marker := range modelmetadata.SourceMarkers {
		t.Run(marker, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "config.json"), `{"architectures":["ExampleForCausalLM"]}`)
			write(t, filepath.Join(dir, "model.safetensors"), "weights")
			write(t, filepath.Join(dir, marker), "Revision:master,CreatedAt:1756691498")
			var c Candidate
			identify(dir, "org/model", &c)
			if c.Source != SourceModelScope || c.Basis != BasisModelScope {
				t.Fatalf("%s alone was not recognized: %+v", marker, c)
			}
			// Only the version marker carries a revision; the plain marker is
			// an empty flag, so it names no revision and none is invented.
			want := ""
			if marker == modelmetadata.ModelScopeVersionMarker {
				want = "master"
			}
			if c.Revision != want {
				t.Fatalf("%s left revision %q, want %q", marker, c.Revision, want)
			}
		})
	}
}
