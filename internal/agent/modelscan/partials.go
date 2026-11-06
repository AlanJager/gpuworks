package modelscan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// A transfer by internal/agent/download writes x.part, with x.part.state
// recording x's full size when it fetches in ranges, and finishes by renaming
// x.part to x. Both standing beside a complete x therefore mean another tool
// wrote x after the platform's transfer stopped (a cancel keeps the sidecars
// to resume from): they are left over, not a transfer in flight.

// partStateLimit bounds the sidecar read; it holds a URL, a size and one
// offset per range.
const partStateLimit = 64 << 10

// staleDownloadSidecar reports whether path, a ".part" or ".part.state" file,
// is left over beside a finished file: x.part.state names a size and x is a
// regular file of exactly that size. A sidecar without a readable state, or
// beside a file of any other size, is a transfer that did not finish.
func staleDownloadSidecar(path string) bool {
	dest := strings.TrimSuffix(strings.TrimSuffix(path, ".state"), ".part")
	size, ok := recordedPartSize(dest + ".part.state")
	if !ok {
		return false
	}
	info, err := os.Stat(dest)
	return err == nil && info.Mode().IsRegular() && info.Size() == size
}

func recordedPartSize(statePath string) (int64, bool) {
	raw, ok := readCapped(statePath, partStateLimit)
	if !ok {
		return 0, false
	}
	var state struct {
		Size int64 `json:"size"`
	}
	if json.Unmarshal(raw, &state) != nil || state.Size <= 0 {
		return 0, false
	}
	return state.Size, true
}

// indexLimit bounds a sharded checkpoint's index read; a 2,000-shard index
// with tens of thousands of tensors stays well under it.
const indexLimit = 64 << 20

// shardsPresent reports whether dir holds every shard its
// model.safetensors.index.json names, each a non-empty regular file. The
// Hugging Face CLI leaves *.incomplete files under .cache/huggingface with
// hashed names that say nothing about which file they were; with every
// shard in place they are left over rather than a download in flight.
func shardsPresent(dir string) bool {
	raw, ok := readCapped(filepath.Join(dir, "model.safetensors.index.json"), indexLimit)
	if !ok {
		return false
	}
	var index struct {
		WeightMap map[string]string `json:"weight_map"`
	}
	if json.Unmarshal(raw, &index) != nil || len(index.WeightMap) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, shard := range index.WeightMap {
		if seen[shard] {
			continue
		}
		seen[shard] = true
		if shard == "" || filepath.IsAbs(shard) || strings.Contains(filepath.ToSlash(shard), "..") {
			return false
		}
		info, err := os.Stat(filepath.Join(dir, shard))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return false
		}
	}
	return true
}

// RemoveStaleDownloadSidecars removes the x.part and x.part.state left beside
// each file of files (paths relative to dir) that is complete: complete
// reports whether the file at that path is the size its source lists. It
// never removes the files themselves, and returns the sidecars it removed,
// relative to dir.
func RemoveStaleDownloadSidecars(dir string, files []string, complete func(rel string, info os.FileInfo) bool) []string {
	var removed []string
	for _, rel := range files {
		dest := filepath.Join(dir, filepath.FromSlash(rel))
		info, err := os.Stat(dest)
		if err != nil || !info.Mode().IsRegular() || !complete(rel, info) {
			continue
		}
		for _, suffix := range []string{".part", ".part.state"} {
			if err := os.Remove(dest + suffix); err == nil {
				removed = append(removed, rel+suffix)
			}
		}
	}
	return removed
}
