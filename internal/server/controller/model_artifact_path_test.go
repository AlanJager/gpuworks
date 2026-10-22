package controller

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestRelModelPathSeparatesWholeRepositoryAndSparseSubfolder(t *testing.T) {
	whole := &store.Model{Source: "modelscope", SourceID: "MiniMax/MiniMax-H3", Revision: "commit"}
	sparse := *whole
	sparse.ArtifactSubfolder = "FL2VA"

	wholePath := relModelPath(whole)
	sparsePath := relModelPath(&sparse)
	if wholePath == sparsePath {
		t.Fatalf("whole and sparse artifacts share path %q", wholePath)
	}
	if strings.Contains(sparsePath, "FL2VA") {
		t.Fatalf("subfolder leaked into node path: %q", sparsePath)
	}
	if sparsePath != relModelPath(&sparse) {
		t.Fatalf("sparse artifact path is not deterministic: %q != %q", sparsePath, relModelPath(&sparse))
	}
}
