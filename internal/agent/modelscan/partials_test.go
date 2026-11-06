package modelscan

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The sidecars internal/agent/download leaves on a cancel: x.part and
// x.part.state recording x's full size ({"url","size","chunks"}).
func writeSidecars(t *testing.T, dest string, size int) {
	t.Helper()
	write(t, dest+".part", "part")
	write(t, dest+".part.state", `{"url":"https://huggingface.co/org/m/resolve/abc/x","size":`+strconv.Itoa(size)+`,"chunks":[100,200,300,400]}`)
}

// A transfer cancelled by the platform and finished by another tool leaves
// its sidecars beside a complete file: the directory is not unfinished.
// Anything short of that proof still is, and says what to do about it.
func TestValidateModelDirTellsLeftoverSidecarsFromAnUnfinishedDownload(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(t *testing.T, dir string)
		unfinished string // the file the error names, "" when the directory is valid
	}{
		{"finished by another tool", func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "model-00001.safetensors"), "0123456789")
			writeSidecars(t, filepath.Join(dir, "model-00001.safetensors"), 10)
		}, ""},
		{"still short of its size", func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "model-00001.safetensors"), "01234")
			writeSidecars(t, filepath.Join(dir, "model-00001.safetensors"), 10)
		}, "model-00001.safetensors.part"},
		{"never finished", func(t *testing.T, dir string) {
			writeSidecars(t, filepath.Join(dir, "model-00001.safetensors"), 10)
		}, "model-00001.safetensors.part"},
		{"unreadable state", func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "model-00001.safetensors"), "0123456789")
			write(t, filepath.Join(dir, "model-00001.safetensors.part.state"), "{not json")
		}, "model-00001.safetensors.part.state"},
		{"a single-stream part has no state to prove it", func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "config.json"), "{}")
			write(t, filepath.Join(dir, "config.json.part"), "{")
		}, "config.json.part"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "config.json"), "{}")
			tc.setup(t, dir)
			size, err := ValidateModelDir(dir)
			if tc.unfinished == "" {
				if err != nil || size == 0 {
					t.Fatalf("size=%d err=%v, want a valid directory", size, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "unfinished download: "+tc.unfinished+" ") || !strings.Contains(err.Error(), "remove the model from this node") {
				t.Fatalf("err=%v, want %s named with what to do", err, tc.unfinished)
			}
		})
	}
}

// The storage scan applies the same proof, and lets the Hugging Face CLI's
// hashed *.incomplete leftovers be once every indexed shard is in place.
func TestScanDoesNotHoldLeftoversAgainstACompleteModel(t *testing.T) {
	root := t.TempDir()
	index := `{"metadata":{},"weight_map":{"a":"model-00001-of-00002.safetensors","b":"model-00002-of-00002.safetensors","c":"model-00002-of-00002.safetensors"}}`
	for _, rel := range []string{"org/finished", "org/missing-shard"} {
		hfModel(t, root, rel)
		write(t, p(root, rel+"/model.safetensors.index.json"), index)
		write(t, p(root, rel+"/model-00001-of-00002.safetensors"), "0123456789")
		write(t, p(root, rel+"/.cache/huggingface/download/Hv2_5e1c.0f3a9b.incomplete"), "partial")
	}
	// hfModel writes both shards; the second model loses one.
	if err := os.Remove(p(root, "org/missing-shard/model-00002-of-00002.safetensors")); err != nil {
		t.Fatal(err)
	}
	writeSidecars(t, p(root, "org/finished/model-00001-of-00002.safetensors"), 10)
	res, err := Scan(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := byRel(res)
	if c := got["org/finished"]; c.Incomplete {
		t.Fatalf("finished model flagged: %q", c.IncompleteReason)
	}
	if c := got["org/missing-shard"]; !c.Incomplete || !strings.Contains(c.IncompleteReason, "*.incomplete present") {
		t.Fatalf("model with a missing shard: incomplete=%v reason=%q", c.Incomplete, c.IncompleteReason)
	}
}

// Removal takes only the sidecars beside files their source calls complete,
// never the files.
func TestRemoveStaleDownloadSidecarsKeepsFilesAndUnfinishedTransfers(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.safetensors"), "0123456789")
	writeSidecars(t, filepath.Join(dir, "a.safetensors"), 10)
	write(t, filepath.Join(dir, "sub/b.safetensors"), "01234")
	writeSidecars(t, filepath.Join(dir, "sub/b.safetensors"), 10)
	listed := map[string]int64{"a.safetensors": 10, "sub/b.safetensors": 10, "missing.safetensors": 3}
	removed := RemoveStaleDownloadSidecars(dir, []string{"a.safetensors", "sub/b.safetensors", "missing.safetensors"}, func(rel string, info os.FileInfo) bool {
		return info.Size() == listed[rel]
	})
	if strings.Join(removed, ",") != "a.safetensors.part,a.safetensors.part.state" {
		t.Fatalf("removed = %v", removed)
	}
	for _, kept := range []string{"a.safetensors", "sub/b.safetensors", "sub/b.safetensors.part", "sub/b.safetensors.part.state"} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Fatalf("%s was removed: %v", kept, err)
		}
	}
}
