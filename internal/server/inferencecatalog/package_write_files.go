package inferencecatalog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

func copyPackageModelFiles(destination string, catalog FlatCatalog, source fs.FS) error {
	for _, model := range catalog.Models {
		files := map[string]struct{}{}
		for _, spec := range model.Specs {
			for _, asset := range spec.Assets {
				files[asset.File] = struct{}{}
			}
		}
		for _, readme := range []string{"README.zh-CN.md", "README.en.md"} {
			if source != nil {
				if _, err := fs.Stat(source, "models/"+model.ModelID+"/"+readme); err == nil {
					files[readme] = struct{}{}
				} else if !errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("inspect model %s file %s: %w", model.ModelID, readme, err)
				}
			}
		}
		for name := range files {
			if source == nil {
				return fmt.Errorf("model %s file %s requires a package files source", model.ModelID, name)
			}
			sourceName := path.Join("models", model.ModelID, name)
			info, err := fs.Stat(source, sourceName)
			if err != nil {
				return fmt.Errorf("inspect model %s file %s: %w", model.ModelID, name, err)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("model %s file %s is not regular", model.ModelID, name)
			}
			data, err := fs.ReadFile(source, sourceName)
			if err != nil {
				return fmt.Errorf("read model %s file %s: %w", model.ModelID, name, err)
			}
			target := filepath.Join(destination, filepath.FromSlash(sourceName))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("create model %s file directory: %w", model.ModelID, err)
			}
			if err := os.WriteFile(target, data, 0o644); err != nil {
				return fmt.Errorf("write model %s file %s: %w", model.ModelID, name, err)
			}
		}
	}
	return nil
}
