package modelscan

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
func writeMistral(t *testing.T, dir string) {
	t.Helper()
	write(t, filepath.Join(dir, "params.json"), `{"dim":4096,"n_layers":32}`)
	write(t, filepath.Join(dir, "tekken.json"), `{"vocab":[]}`)
	write(t, filepath.Join(dir, "consolidated.safetensors"), "weights")
}

func TestDiscoveryDoesNotBorrowDescendantWeights(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "config.json"), `{"architectures":["ExampleForCausalLM"]}`)
	hfModel(t, root, "child")
	report, err := ParseMetadata(context.Background(), root)
	if err != nil || len(report.Entries) != 0 {
		t.Fatalf("parent borrowed child weights: %+v %v", report, err)
	}
	res, err := Scan(context.Background(), root, Options{})
	if err != nil || len(res.Candidates) != 1 || res.Candidates[0].RelPath != "child" {
		t.Fatalf("child not discovered: %+v %v", res, err)
	}
}

func TestDiscoveryNativeMistralAndRootSymlink(t *testing.T) {
	dir := t.TempDir()
	writeMistral(t, dir)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	report, err := ParseMetadata(context.Background(), link)
	if err != nil || len(report.Entries) != 1 || report.Entries[0].Format != "mistral" {
		t.Fatalf("native entry: %+v %v", report, err)
	}
	if err := os.Remove(filepath.Join(dir, "tekken.json")); err != nil {
		t.Fatal(err)
	}
	report, err = ParseMetadata(context.Background(), dir)
	if err != nil || len(report.Entries) != 0 {
		t.Fatalf("partial native entry: %+v %v", report, err)
	}
}
