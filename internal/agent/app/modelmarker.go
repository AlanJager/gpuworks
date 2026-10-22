package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

func validCompletionMarker(dir string, task proto.ModelFileTask) (int64, bool) {
	_, total, ok := validatedMarker(dir, task)
	return total, ok
}

// validatedMarker is validCompletionMarker that also returns the marker it
// read, so a caller that needs both reads the share once.
func validatedMarker(dir string, task proto.ModelFileTask) (modelCompletion, int64, bool) {
	subfolder, err := modelrepo.NormalizeSubfolder(task.Subfolder)
	if err != nil {
		return modelCompletion{}, 0, false
	}
	marker, ok := readCompletionMarker(dir)
	if !ok || (marker.Version != 1 && marker.Version != 2) ||
		marker.Source != task.Source || marker.SourceID != task.SourceID ||
		marker.Revision != normalizedRevision(task.Source, task.Revision) || marker.Subfolder != subfolder {
		return modelCompletion{}, 0, false
	}
	total, err := validateRepoManifest(marker.Files)
	if err != nil || total != marker.Bytes {
		return modelCompletion{}, 0, false
	}
	present, _ := repoFilesPresent(dir, marker.Files, false)
	if !present {
		return modelCompletion{}, 0, false
	}
	return marker, total, true
}

func readCompletionMarker(dir string) (modelCompletion, bool) {
	b, err := os.ReadFile(filepath.Join(dir, modelCompleteMarker))
	if err != nil {
		return modelCompletion{}, false
	}
	var marker modelCompletion
	if json.Unmarshal(b, &marker) != nil {
		return modelCompletion{}, false
	}
	return marker, true
}

func manifestRevision(files []modelrepo.File) string {
	for _, file := range files {
		if revision := strings.TrimSpace(file.Revision); revision != "" {
			return revision
		}
	}
	return ""
}

// writeCompletionMarker records what actually landed on disk and returns that
// measured total.
//
// The manifest is stat'ed rather than copied from the listing, so the marker
// is an exact record even when the listing was not — which is what lets the
// marker check stay an exact comparison for every source. For a listing that
// was already exact this is the identity.
func writeCompletionMarker(dir string, task proto.ModelFileTask, files []modelrepo.File) (int64, error) {
	subfolder, err := modelrepo.NormalizeSubfolder(task.Subfolder)
	if err != nil {
		return 0, err
	}
	measured := make([]modelrepo.File, 0, len(files))
	var total int64
	for _, file := range files {
		path, err := modelRepoDestination(dir, file.Path)
		if err != nil {
			return 0, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return 0, err
		}
		if !info.Mode().IsRegular() {
			return 0, fmt.Errorf("%s is not a regular file", file.Path)
		}
		file.Size = info.Size()
		measured = append(measured, file)
		total += file.Size
	}
	marker := modelCompletion{
		Version: 2, Source: task.Source, SourceID: task.SourceID,
		Revision: normalizedRevision(task.Source, task.Revision), Subfolder: subfolder, Files: measured, Bytes: total, Created: time.Now().UTC(),
	}
	b, err := json.Marshal(marker)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(dir, ".gpuworks-model-complete-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, modelCompleteMarker)); err != nil {
		return 0, err
	}
	return total, nil
}

// isModelPresent heuristically checks whether a model dir already holds weights.
func isModelPresent(dir string) bool {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return false
	}
	hasConfig, hasWeights := false, false
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if name == "config.json" {
			hasConfig = true
		}
		if filepath.Ext(name) == ".safetensors" || filepath.Ext(name) == ".gguf" || filepath.Ext(name) == ".bin" || filepath.Ext(name) == ".pt" {
			hasWeights = true
		}
		return nil
	})
	return err == nil && hasConfig && hasWeights
}
