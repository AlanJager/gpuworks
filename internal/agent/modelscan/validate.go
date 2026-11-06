// Package modelscan walks a model library (for example a shared NFS export),
// finds model directories through modelmetadata and works out where each one
// came from. It also owns the layout-independent finished-directory check.
package modelscan

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CompletionMarker is the file a finished gpuworks download leaves in a model
// directory.
const CompletionMarker = ".gpuworks-model-complete.json"

// errEmptyModelDir is returned for a directory that holds no model bytes.
var errEmptyModelDir = errors.New("model directory holds no files")

// ValidateModelDir checks that dir is a finished model directory and returns
// the total size of all regular files below it.
//
// It deliberately knows no layout. What a directory must contain is the
// engine's business, and an inference template names the engine: requiring
// config.json here would refuse a GGUF-only repository, or any layout a future
// engine brings, after it was fully downloaded. Whether every file arrived is
// proven where the files came from (the repository manifest, the copy source
// listing, the peer archive digest), not by guessing at names. What is left
// for this check is what holds for every layout: the directory is readable,
// holds at least one non-empty file, and carries no unfinished transfer.
func ValidateModelDir(dir string) (int64, error) {
	// WalkDir lstats the root, so a root that is itself a symlink yields just
	// the link entry and the directory looks empty. Resolve it first: model
	// paths legitimately arrive as symlinks to a versioned download dir.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return 0, err
	}
	var total, payload int64
	err = filepath.WalkDir(resolved, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		// A downloader's in-flight file (see internal/agent/download) means the
		// directory is still being written, whatever else it holds - unless it
		// is left over beside the file another tool finished.
		if strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".part.state") {
			if staleDownloadSidecar(path) {
				return nil
			}
			rel, _ := filepath.Rel(resolved, path)
			return fmt.Errorf("model directory has an unfinished download: %s (a download into it is still running or was interrupted; resume the download, or remove the model from this node and download it again)", filepath.ToSlash(rel))
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if name == CompletionMarker {
			return nil
		}
		// A snapshot directory links its files to shared blobs; what counts
		// is the bytes behind the link.
		if info.Mode()&os.ModeSymlink != 0 {
			if target, statErr := os.Stat(path); statErr == nil {
				info = target
			}
		}
		if info.Mode().IsRegular() {
			payload += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if payload == 0 {
		return 0, errEmptyModelDir
	}
	return total, nil
}
