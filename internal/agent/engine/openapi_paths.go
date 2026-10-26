package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
)

// readOpenAPIPaths reads the path keys from the engine's OpenAPI document.
// A nil result means the document was not read: the request failed, the status
// was not 200, or the body was not an OpenAPI object with a paths map. An
// empty slice means the document listed no paths.
func readOpenAPIPaths(ctx context.Context, client *http.Client, port int) *[]string {
	if port <= 0 || client == nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/openapi.json", port), nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var doc struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil || doc.Paths == nil {
		return nil
	}
	paths := make([]string, 0, len(doc.Paths))
	for path := range doc.Paths {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return &paths
}
