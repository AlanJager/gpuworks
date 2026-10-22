// Package clean is apiconv's fixture of responses that follow every
// convention: it must yield no violation and no opaque field.
package clean

import (
	"encoding/json"
	"net/http"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// List mirrors dto.List.
type List[T any] struct {
	Items []T `json:"items"`
	Total int `json:"total"`
}

// Node is a well-formed element with a nested struct.
type Node struct {
	UUID   string            `json:"uuid"`
	GPUs   []GPU             `json:"gpus"`
	Labels map[string]string `json:"labels,omitempty"`
}

// GPU is Node's nested element.
type GPU struct {
	Index    int   `json:"index"`
	MemoryMB int64 `json:"memory_mb"`
}

func nodes(w http.ResponseWriter) { writeJSON(w, 200, List[Node]{Items: []Node{}}) }

func node(w http.ResponseWriter) { writeJSON(w, 200, &Node{}) }

func status(w http.ResponseWriter) { writeJSON(w, 200, map[string]string{"status": "ok"}) }
