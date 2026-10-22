package testenv

import (
	"os"
	"path/filepath"
)

// loadFakeModelFixture finds output from test/fake-model without assuming a
// particular package working directory. It is deliberately best-effort: the
// generator output is ignored by git and may not exist on a clean checkout.
func loadFakeModelFixture() map[string][]byte {
	wd, err := os.Getwd()
	if err != nil {
		return nil
	}
	for dir := wd; ; {
		root := filepath.Join(dir, "test", "fake-model", "testdata", "qwen3-4b-fake")
		entries, readErr := os.ReadDir(root)
		if readErr == nil {
			files := make(map[string][]byte)
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				data, fileErr := os.ReadFile(filepath.Join(root, entry.Name()))
				if fileErr != nil {
					return nil
				}
				files[entry.Name()] = data
			}
			if _, ok := files["model.safetensors.index.json"]; ok {
				return files
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return nil
}
