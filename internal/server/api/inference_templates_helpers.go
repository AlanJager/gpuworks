package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

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

func (a *apiHandlers) readInferenceTemplateDocumentation(model store.InferenceModel, file string) string {
	if strings.TrimSpace(file) == "" {
		return ""
	}
	var data []byte
	var err error
	if a.d.CatalogPackages != nil && model.PackageDigest != "" {
		data, err = a.d.CatalogPackages.ReadDocumentation(model.PackageDigest, model.ModelID, file)
	}
	if err != nil || len(data) > 512*1024 {
		return ""
	}
	return string(data)
}
