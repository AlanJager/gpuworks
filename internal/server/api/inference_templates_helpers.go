package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func sortedKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func inferenceTemplateETag(models []store.InferenceModel, query string) string {
	data, _ := json.Marshal(models)
	digest := sha256.Sum256(append(data, query...))
	return `"` + hex.EncodeToString(digest[:]) + `"`
}
