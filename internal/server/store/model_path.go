package store

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// ModelArtifactPath is the node-relative directory a model's weights occupy.
// The deploy API, download API and controller use this preferred layout. When
// another identity already owns the directory, transfer preparation allocates
// a stable model-specific suffix; callers use the returned ModelFile.Path.
//
// An empty revision retains the historical "main" directory spelling; that is
// a layout convention, not a statement about the provider's default branch.
// Revision identity remains separate from the preferred directory. A sparse artifact
// (one subfolder of a larger repository) gets its own directory keyed by a
// digest of the subfolder, so it never collides with the whole repository while
// keeping the path short and free of nested separators.
func ModelArtifactPath(m *Model) string {
	if m == nil {
		return ""
	}
	source := strings.ToLower(strings.TrimSpace(m.Source))
	if source == "" {
		source = "model"
	}
	id := strings.Trim(strings.ReplaceAll(strings.TrimSpace(m.SourceID), "/", "_"), "_")
	if id == "" {
		id = fmt.Sprintf("model-%d", m.ID)
	}
	revision := strings.TrimSpace(m.Revision)
	if revision == "" {
		revision = "main"
	}
	if subfolder := strings.TrimSpace(m.ArtifactSubfolder); subfolder != "" {
		digest := sha256.Sum256([]byte(subfolder))
		return fmt.Sprintf("%s/%s-%s-subfolder-%x", source, id, revision, digest[:6])
	}
	return fmt.Sprintf("%s/%s-%s", source, id, revision)
}

// SharedModelRelPath is where gpuworks writes a model into a shared storage.
// The default branch of a whole repository lands at its repository id
// (<org>/<name>), the layout people already keep model libraries in; a pinned
// revision or a subfolder gets a suffix so it never overwrites the default.
func SharedModelRelPath(m *Model) string {
	if m == nil {
		return ""
	}
	id := strings.Trim(strings.TrimSpace(m.SourceID), "/")
	if clean, err := storageRelPathClean(id); err == nil && clean != "" && !strings.HasPrefix(clean, ".") {
		id = clean
	} else {
		id = fmt.Sprintf("models/model-%d", m.ID)
	}
	revision := strings.TrimSpace(m.Revision)
	if revision != "" && revision != "main" && revision != "master" {
		id += "-" + strings.ReplaceAll(revision, "/", "_")
	}
	if subfolder := strings.TrimSpace(m.ArtifactSubfolder); subfolder != "" {
		digest := sha256.Sum256([]byte(subfolder))
		id += fmt.Sprintf("-subfolder-%x", digest[:6])
	}
	return id
}
