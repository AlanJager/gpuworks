package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gpuworks.ai/gpuworks/pkg/model/modelmetadata"
)

const copySourceMarker = ".gpuworks-copy-source.json"

type copySourceEntry struct {
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

type copySourceSnapshot struct {
	Source string            `json:"source"`
	Files  []copySourceEntry `json:"files"`
}

// prepareCopyResume trusts partial bytes only when their source identity is
// known. Legacy partials without a snapshot are recopied, never guessed valid
// from size alone. The marker is local task metadata, not a model file.
func prepareCopyResume(source, target string, files []copyFile) error {
	snapshot := copySourceSnapshot{Source: source}
	for _, f := range files {
		snapshot.Files = append(snapshot.Files, copySourceEntry{f.rel, f.size, f.modTime})
	}
	want, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	marker := filepath.Join(target, copySourceMarker)
	old, err := os.ReadFile(marker)
	if err == nil && string(old) == string(want) {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	// Remove only model files named by the previous/current source manifests.
	// Never remove the directory, which may be the final target.
	paths := map[string]bool{}
	var previous copySourceSnapshot
	if len(old) > 0 && json.Unmarshal(old, &previous) != nil {
		return fmt.Errorf("invalid copy source snapshot")
	}
	for _, f := range previous.Files {
		paths[f.Path] = true
	}
	for _, f := range files {
		paths[f.rel] = true
	}
	for rel := range paths {
		path, err := modelRepoDestination(target, rel)
		if err != nil {
			return err
		}
		for _, suffix := range []string{"", ".part"} {
			if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	// Tool metadata is not a model file, so no listing names it and the loop
	// above cannot reach it. A snapshot a pre-skip agent wrote does name it, and
	// that agent left copies of it in the target; drop them so a resumed copy
	// ends with the same file set a fresh one would, and so the stale copies
	// cannot later be read back as this model's source identity.
	for _, name := range modelmetadata.SourceMarkers {
		if err := os.Remove(filepath.Join(target, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Remove(filepath.Join(target, modelCompleteMarker)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(marker+".part", want, 0o600); err != nil {
		return err
	}
	return os.Rename(marker+".part", marker)
}
