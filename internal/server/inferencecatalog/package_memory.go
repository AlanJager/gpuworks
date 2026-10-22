package inferencecatalog

import (
	"fmt"
	"io/fs"
	"path"
	"strings"
	"testing/fstest"
	"time"

	"gopkg.in/yaml.v3"
)

// BuildPackageFS assembles and validates a package in memory. Runtime callers
// use it for small content-addressed derivatives such as a user clone whose
// model identity changes while its model-local documentation and assets stay
// byte-for-byte identical.
func BuildPackageFS(publishedAt string, catalog FlatCatalog, files map[string][]byte) (fs.FS, error) {
	if err := ValidateFlatCatalog(&catalog); err != nil {
		return nil, err
	}
	publishedAt = strings.TrimSpace(publishedAt)
	if _, err := time.Parse(time.RFC3339, publishedAt); err != nil {
		return nil, fmt.Errorf("inference catalog package published_at must be RFC3339: %w", err)
	}
	manifest := PackageManifest{SchemaVersion: PackageSchemaVersion, PublishedAt: publishedAt, Models: len(catalog.Models), Specs: countSpecs(catalog)}
	manifestData, err := yaml.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("marshal inference catalog package manifest: %w", err)
	}
	memory := fstest.MapFS{"manifest.yaml": &fstest.MapFile{Data: manifestData, Mode: 0o644}}
	for _, model := range catalog.Models {
		name, err := modelFileName(model.ModelID)
		if err != nil {
			return nil, err
		}
		data, err := MarshalModelYAML(model)
		if err != nil {
			return nil, fmt.Errorf("marshal inference catalog model %s: %w", model.ModelID, err)
		}
		memory[name] = &fstest.MapFile{Data: data, Mode: 0o644}
	}
	for rawName, data := range files {
		name := path.Clean(strings.ReplaceAll(strings.TrimSpace(rawName), "\\", "/"))
		if name != rawName || !fs.ValidPath(name) || name == "manifest.yaml" || path.Base(name) == "model.yaml" {
			return nil, fmt.Errorf("invalid inference catalog package file %q", rawName)
		}
		if _, duplicate := memory[name]; duplicate {
			return nil, fmt.Errorf("duplicate inference catalog package file %q", name)
		}
		memory[name] = &fstest.MapFile{Data: append([]byte(nil), data...), Mode: 0o644}
	}
	if _, err := LoadPackage(memory); err != nil {
		return nil, fmt.Errorf("validate assembled inference catalog package: %w", err)
	}
	return memory, nil
}
