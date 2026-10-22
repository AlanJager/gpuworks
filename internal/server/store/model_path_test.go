package store

import (
	"strings"
	"testing"
)

// Every producer of a ModelFile.Path must agree on this layout, otherwise a
// model downloaded through one code path is invisible to the others.
func TestModelArtifactPathIsStableAcrossRevisionAndSubfolderShapes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model *Model
		want  string
	}{
		{
			name:  "pinned revision",
			model: &Model{Source: "huggingface", SourceID: "Qwen/Qwen3-ASR-1.7B", Revision: "7278e1e7"},
			want:  "huggingface/Qwen_Qwen3-ASR-1.7B-7278e1e7",
		},
		{
			name:  "empty revision resolves to the default branch",
			model: &Model{Source: "modelscope", SourceID: "Qwen/Qwen3-ASR-1.7B"},
			want:  "modelscope/Qwen_Qwen3-ASR-1.7B-main",
		},
		{
			name:  "whitespace-only revision resolves to the default branch",
			model: &Model{Source: "modelscope", SourceID: "Qwen/Qwen3-ASR-1.7B", Revision: "   "},
			want:  "modelscope/Qwen_Qwen3-ASR-1.7B-main",
		},
		{
			name:  "provider case is normalized",
			model: &Model{Source: "HuggingFace", SourceID: "org/model", Revision: "main"},
			want:  "huggingface/org_model-main",
		},
		{
			name:  "nested repository separators are flattened",
			model: &Model{Source: "huggingface", SourceID: "org/team/model", Revision: "main"},
			want:  "huggingface/org_team_model-main",
		},
		{
			// A model imported from an HTTP directory index has no revision to
			// pin, so it lands under the default-branch spelling like any other
			// unpinned model. The directory URL is deliberately not part of the
			// path: two mirrors serving one repository serve the same weights.
			name:  "http directory import has no revision",
			model: &Model{Source: "url", SourceID: "Qwen/Qwen3-0.6B", SourceURL: "https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B"},
			want:  "url/Qwen_Qwen3-0.6B-main",
		},
		{
			name:  "two mirrors of one repository share a directory",
			model: &Model{Source: "url", SourceID: "Qwen/Qwen3-0.6B", SourceURL: "http://mirror.internal:8081/Qwen/Qwen3-0.6B"},
			want:  "url/Qwen_Qwen3-0.6B-main",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ModelArtifactPath(tc.model); got != tc.want {
				t.Fatalf("ModelArtifactPath = %q, want %q", got, tc.want)
			}
		})
	}
}

// Adding a source must not move weights that are already on disk: a changed
// path would orphan every downloaded model and silently re-download it.
func TestModelArtifactPathIsUnchangedForExistingSources(t *testing.T) {
	for _, tc := range []struct {
		model *Model
		want  string
	}{
		{model: &Model{Source: "huggingface", SourceID: "Qwen/Qwen3-0.6B", Revision: "main"}, want: "huggingface/Qwen_Qwen3-0.6B-main"},
		{model: &Model{Source: "huggingface", SourceID: "Qwen/Qwen3-0.6B", Revision: "7278e1e7"}, want: "huggingface/Qwen_Qwen3-0.6B-7278e1e7"},
		{model: &Model{Source: "huggingface", SourceID: "Qwen/Qwen3-0.6B"}, want: "huggingface/Qwen_Qwen3-0.6B-main"},
		{model: &Model{Source: "modelscope", SourceID: "Qwen/Qwen3-0.6B", Revision: "main"}, want: "modelscope/Qwen_Qwen3-0.6B-main"},
		{model: &Model{Source: "modelscope", SourceID: "Qwen/Qwen3-0.6B", Revision: "c3f8a1b2"}, want: "modelscope/Qwen_Qwen3-0.6B-c3f8a1b2"},
		{model: &Model{Source: "local", SourceID: "Qwen3-0.6B"}, want: "local/Qwen3-0.6B-main"},
		{model: &Model{Source: "local", SourceID: "Qwen/Qwen3-0.6B", Revision: "main"}, want: "local/Qwen_Qwen3-0.6B-main"},
		{model: &Model{Source: "modelscope", SourceID: "MiniMax/MiniMax-H3", Revision: "commit", ArtifactSubfolder: "FL2VA"}, want: "modelscope/MiniMax_MiniMax-H3-commit-subfolder-38e5ba23b7ee"},
	} {
		if got := ModelArtifactPath(tc.model); got != tc.want {
			t.Errorf("ModelArtifactPath(%+v) = %q, want %q", tc.model, got, tc.want)
		}
	}
	// A directory URL is identity, not layout: setting it must not move an
	// existing model's weights either.
	withURL := &Model{Source: "huggingface", SourceID: "Qwen/Qwen3-0.6B", Revision: "main", SourceURL: "https://mirror/Qwen/Qwen3-0.6B"}
	if got := ModelArtifactPath(withURL); got != "huggingface/Qwen_Qwen3-0.6B-main" {
		t.Fatalf("SourceURL changed the path: %q", got)
	}
}

// The same repository at the same revision from two providers is stored
// separately: the two copies are only *equivalent*, and conflating their paths
// would let one provider's partial download masquerade as the other's.
func TestModelArtifactPathSeparatesProvidersForTheSameRepository(t *testing.T) {
	hf := ModelArtifactPath(&Model{Source: "huggingface", SourceID: "Qwen/Qwen3-ASR-1.7B", Revision: "main"})
	ms := ModelArtifactPath(&Model{Source: "modelscope", SourceID: "Qwen/Qwen3-ASR-1.7B", Revision: "main"})
	if hf == ms {
		t.Fatalf("cross-provider copies share path %q", hf)
	}
}

func TestModelArtifactPathGivesSparseSubfolderArtifactsTheirOwnDirectory(t *testing.T) {
	whole := &Model{Source: "modelscope", SourceID: "MiniMax/MiniMax-H3", Revision: "commit"}
	sparse := *whole
	sparse.ArtifactSubfolder = "FL2VA"
	other := *whole
	other.ArtifactSubfolder = "AUDIO"

	wholePath := ModelArtifactPath(whole)
	sparsePath := ModelArtifactPath(&sparse)
	if wholePath == sparsePath {
		t.Fatalf("whole and sparse artifacts share path %q", wholePath)
	}
	if sparsePath == ModelArtifactPath(&other) {
		t.Fatalf("two subfolders of one repository share path %q", sparsePath)
	}
	// The subfolder name itself must not leak into the on-disk path: it may
	// contain separators or characters a path segment cannot carry.
	if strings.Contains(sparsePath, "FL2VA") {
		t.Fatalf("subfolder leaked into node path: %q", sparsePath)
	}
	if strings.Count(sparsePath, "/") != 1 {
		t.Fatalf("path has nested segments: %q", sparsePath)
	}
	if sparsePath != ModelArtifactPath(&sparse) {
		t.Fatalf("sparse artifact path is not deterministic: %q", sparsePath)
	}
	// A subfolder with separators still yields one flat segment.
	nested := *whole
	nested.ArtifactSubfolder = "a/b/c"
	if got := ModelArtifactPath(&nested); strings.Count(got, "/") != 1 {
		t.Fatalf("nested subfolder produced %q", got)
	}
}

// A model row with no usable repository id must still get a unique, stable
// directory rather than colliding with every other such row.
func TestModelArtifactPathFallsBackToTheModelIDWithoutARepository(t *testing.T) {
	first := ModelArtifactPath(&Model{Source: "local", SourceID: "  ", ID: 7})
	if first != "local/model-7-main" {
		t.Fatalf("fallback path = %q", first)
	}
	second := ModelArtifactPath(&Model{Source: "local", SourceID: "///", ID: 8})
	if second == first {
		t.Fatalf("two id-less models share path %q", first)
	}
	if got := ModelArtifactPath(&Model{SourceID: "org/model", Revision: "main"}); !strings.HasPrefix(got, "model/") {
		t.Fatalf("missing provider path = %q", got)
	}
	if got := ModelArtifactPath(nil); got != "" {
		t.Fatalf("nil model path = %q", got)
	}
}
