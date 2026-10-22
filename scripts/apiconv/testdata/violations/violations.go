// Package violations is apiconv's fixture: one response per rule the checker
// enforces, plus responses it must accept.
package violations

import (
	"encoding/json"
	"net/http"
)

// writeJSON is the response writer apiconv inspects, as in the api package.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// List mirrors dto.List.
type List[T any] struct {
	Items []T `json:"items"`
	Total int `json:"total"`
}

// Row is a well-formed element.
type Row struct {
	ID       int    `json:"id"`
	NodeName string `json:"node_name,omitempty"`
	Skipped  string `json:"-"`
	internal int
}

// Bad has one untagged field and one PascalCase-tagged field.
type Bad struct {
	Untagged string
	Pascal   int `json:"PascalCase"`
}

// Opaque has fields whose content its types do not show.
type Opaque struct {
	Extra any             `json:"extra"`
	Raw   json.RawMessage `json:"raw"`
	Named string          `json:"named"`
}

// Custom chooses its own encoding, so its field names are not inspected.
type Custom struct {
	Whatever int
}

func (Custom) MarshalJSON() ([]byte, error) { return []byte(`{"custom":1}`), nil }

func listEnvelope(w http.ResponseWriter) { writeJSON(w, 200, List[Row]{Items: []Row{}}) }

func bareSlice(w http.ResponseWriter) { writeJSON(w, 200, []Row{}) }

func rawBytes(w http.ResponseWriter) { writeJSON(w, 200, []byte("{}")) }

func badFields(w http.ResponseWriter) { writeJSON(w, 200, &Bad{}) }

func mapKeys(w http.ResponseWriter) {
	writeJSON(w, 200, map[string]any{"ok_key": 1, "BadKey": 2})
}

func opaque(w http.ResponseWriter) { writeJSON(w, 200, Opaque{}) }

func custom(w http.ResponseWriter) { writeJSON(w, 200, Custom{}) }
