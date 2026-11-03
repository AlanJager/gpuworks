// Package catalogfixture assembles source overlays for offline Go contract
// tests. It does not replace or verify the Python import tool's runtime
// inspection and atomic publishing, which have a separate explicit test lane.
package catalogfixture

import (
	"gopkg.in/yaml.v3"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// WriteLaunchOverlay derives fixture model facts from the live seed instead of
// embedding another copy. Images deliberately use a test-only placeholder.
func WriteLaunchOverlay(t *testing.T, seedRoot, overlayRoot, output string) {
	t.Helper()
	root := filepath.Join(overlayRoot, "models")
	models, specs := 0, 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "model.yaml" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var overlay map[string]any
		if err := yaml.Unmarshal(raw, &overlay); err != nil {
			return err
		}
		if len(overlay) != 2 || overlay["model_id"] == nil || overlay["specs"] == nil {
			t.Fatal("source overlay must contain only model identity and launch recipes")
		}
		mid := overlay["model_id"].(string)
		raw, err = os.ReadFile(filepath.Join(seedRoot, "models", mid, "model.yaml"))
		if err != nil {
			return err
		}
		var model map[string]any
		if err := yaml.Unmarshal(raw, &model); err != nil {
			return err
		}
		seedSpecs := model["specs"].([]any)
		generated := []any{}
		for _, item := range overlay["specs"].([]any) {
			recipe := item.(map[string]any)
			for _, fact := range []string{"weights", "weight_bytes", "model_config", "kv_bytes_per_token", "display_profile"} {
				if _, exists := recipe[fact]; exists {
					t.Fatalf("overlay contains model fact or retired field %s", fact)
				}
			}
			var selected map[string]any
			for _, candidate := range seedSpecs {
				spec := candidate.(map[string]any)
				if spec["name"] == recipe["seed_spec"] {
					selected = spec
					break
				}
			}
			if selected == nil {
				t.Fatal("overlay references a missing seed spec")
			}
			delete(recipe, "seed_spec")
			recipe["weights"], recipe["weight_bytes"] = selected["weights"], selected["weight_bytes"]
			recipe["image"] = "verified.example/amd:" + recipe["engine"].(string)
			generated = append(generated, recipe)
		}
		model["source"], model["specs"] = "user", generated
		raw, err = yaml.Marshal(model)
		if err != nil {
			return err
		}
		dest := filepath.Join(output, "models", mid, "model.yaml")
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(dest, raw, 0600); err != nil {
			return err
		}
		models++
		specs += len(generated)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := yaml.Marshal(map[string]any{"schema_version": 1, "published_at": "2026-11-03T00:00:00Z", "models": models, "specs": specs})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "manifest.yaml"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
}
