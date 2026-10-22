package proto

import (
	"path/filepath"
	"testing"
)

func TestStagingRelPath(t *testing.T) {
	if got := StagingRelPath("cp-a", "Qwen/Qwen3-8B"); got != ".gpuworks/incoming/cp-a/Qwen/Qwen3-8B" {
		t.Fatalf("staging = %q", got)
	}
	if got := StagingRelPath("cp-a", "Qwen//Qwen3-8B/"); got != ".gpuworks/incoming/cp-a/Qwen/Qwen3-8B" {
		t.Fatalf("an unclean target must be cleaned, got %q", got)
	}
}

func TestJoinWithin(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "mnt", "nas")
	for rel, want := range map[string]string{
		"Qwen/Qwen3-8B":    filepath.Join(root, "Qwen", "Qwen3-8B"),
		"a/../b":           filepath.Join(root, "b"),
		"./x/":             filepath.Join(root, "x"),
		".gpuworks/in/x":   filepath.Join(root, ".gpuworks", "in", "x"),
		"..hidden/model":   filepath.Join(root, "..hidden", "model"),
		"weights..v2/last": filepath.Join(root, "weights..v2", "last"),
	} {
		got, err := JoinWithin(root, rel)
		if err != nil || got != want {
			t.Errorf("JoinWithin(%q) = %q, %v; want %q", rel, got, err, want)
		}
	}
	for _, rel := range []string{"", "  ", ".", "./", "/", "/etc", "/mnt/nas/x", "..", "../x", "a/../../x", "a/../..", "x\x00y"} {
		if got, err := JoinWithin(root, rel); err == nil {
			t.Errorf("JoinWithin(%q) = %q, want an error", rel, got)
		}
	}
	if _, err := JoinWithin("", "x"); err == nil {
		t.Error("an empty root was accepted")
	}
}
