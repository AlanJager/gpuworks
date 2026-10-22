package app

import (
	"strings"
	"testing"
)

func TestCheckModelDownloadSpaceRejectsInvalidSize(t *testing.T) {
	err := checkModelDownloadSpace(t.TempDir(), -1)
	if err == nil || !strings.Contains(err.Error(), "invalid expected download size") {
		t.Fatalf("error = %v, want invalid-size error", err)
	}
}

func TestCheckModelDownloadSpaceUsesExistingParent(t *testing.T) {
	root := t.TempDir()
	// A not-yet-created model path must be checked against the filesystem of
	// its nearest existing parent rather than failing because the leaf is absent.
	if err := checkModelDownloadSpace(root+"/nested/model", 1); err != nil {
		t.Fatalf("space check for absent target = %v", err)
	}
}
