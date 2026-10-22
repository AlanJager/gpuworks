package inferencecatalog

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// ModelDeclaringWeightRepo finds the row id of the catalog model that declares
// repoID among its specs' weight sources.
//
// A catalog model is one KV figure with several weight repositories under it: a
// quantized release is a different repository serving the same cache layout,
// and the catalog says so by declaring it as a source. Matching a registered
// model on its own id alone therefore misses every quantized release.
//
// Each spec's weight_sources column is decoded as []WeightSource and compared
// on RepoID alone: surrounding whitespace is ignored, case is folded and the
// whole name must match. A spec whose column does not decode is skipped, so
// one bad row cannot hide a declaration elsewhere. Nested component sources
// are not consulted. When several models declare the repository the one with
// the smallest ID wins, independent of the order of specs.
func ModelDeclaringWeightRepo(specs []store.InferenceSpecWeightSources, repoID string) (uint, bool) {
	key := WeightRepoKey(repoID)
	if key == "" {
		return 0, false
	}
	id, ok := WeightRepoIndex(specs)[key]
	return id, ok
}

// WeightRepoIndex is ModelDeclaringWeightRepo for every repository at once:
// each declared repository, keyed by WeightRepoKey, maps to the smallest ID of
// the models declaring it, under the same decoding rules. Planning asks this
// on every launch it plans, so it is built once per catalog version
// (store.WeightRepoIndex) instead of decoding the whole catalog per question.
func WeightRepoIndex(specs []store.InferenceSpecWeightSources) map[string]uint {
	out := map[string]uint{}
	for _, spec := range specs {
		sources, err := DecodeWeightSources(spec.WeightSources)
		if err != nil {
			continue
		}
		for _, source := range sources {
			key := WeightRepoKey(source.RepoID)
			if key == "" {
				continue
			}
			if have, ok := out[key]; !ok || spec.ModelID < have {
				out[key] = spec.ModelID
			}
		}
	}
	return out
}

// WeightRepoKey is how WeightRepoIndex keys a repository: trimmed and
// case-folded, as ModelDeclaringWeightRepo compares it.
func WeightRepoKey(repoID string) string {
	return strings.ToLower(strings.TrimSpace(repoID))
}
