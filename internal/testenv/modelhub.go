package testenv

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// ModelHub is an offline HuggingFace/ModelScope-compatible repository server.
// Endpoint can be passed directly to modelmeta.Fetch or modelrepo helpers.
type ModelHub struct {
	Server               *httptest.Server
	Endpoint             string
	Repo                 string
	Revision             string // HuggingFace revision; the fake only serves main.
	ModelScopeDefaultRef string
	CommitSHA            string
	WeightBytes          int64
	files                map[string][]byte
}

// FakeModelHub serves a small Qwen3-shaped repository and supports byte ranges,
// which makes it suitable for both metadata and resumable-download tests.
func FakeModelHub(t testing.TB) *ModelHub {
	t.Helper()
	repo, revision := "zstackai/qwen3-4b-fake", "main"
	idx := `{"metadata":{"total_size":8589934592},"weight_map":{"model.embed_tokens.weight":"model-00001-of-00003.safetensors","model.layers.0.self_attn.q_proj.weight":"model-00002-of-00003.safetensors","model.norm.weight":"model-00003-of-00003.safetensors"}}` + "\n"
	files := map[string][]byte{
		"config.json":                  []byte(`{"architectures":["Qwen3ForCausalLM"],"model_type":"qwen3","hidden_size":2560,"num_hidden_layers":36,"num_attention_heads":32,"num_key_value_heads":8,"head_dim":128,"max_position_embeddings":40960,"vocab_size":151936,"torch_dtype":"bfloat16"}` + "\n"),
		"tokenizer.json":               []byte(`{"version":"1.0","model":{"type":"BPE","vocab":{},"merges":[]}}` + "\n"),
		"tokenizer_config.json":        []byte(`{"model_type":"qwen3"}` + "\n"),
		"generation_config.json":       []byte(`{"do_sample":true,"temperature":0.7}` + "\n"),
		"README.md":                    []byte("fake Qwen3 fixture; metadata total_size is intentionally 8 GiB\n"),
		"model.safetensors.index.json": []byte(idx),
	}
	for i := 1; i <= 3; i++ {
		files[fmt.Sprintf("model-%05d-of-00003.safetensors", i)] = make([]byte, 8<<20)
	}
	if generated := loadFakeModelFixture(); generated != nil {
		// `make fake-model` produces the canonical legal safetensors fixture.
		// Keep the tiny in-code fallback so default tests remain zero-setup when
		// the optional ignored testdata directory has not been generated.
		files = generated
	}
	declared := int64(8 << 30)
	var index struct {
		Metadata struct {
			TotalSize int64 `json:"total_size"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(files["model.safetensors.index.json"], &index); err == nil && index.Metadata.TotalSize > 0 {
		declared = index.Metadata.TotalSize
	}
	h := &ModelHub{
		Repo: repo, Revision: revision, ModelScopeDefaultRef: "master",
		CommitSHA: "25d1d1fa49de445ab1f3a41f3537d7dad222826e", WeightBytes: declared, files: files,
	}
	h.Server = httptest.NewServer(http.HandlerFunc(h.serve))
	h.Endpoint = h.Server.URL
	t.Cleanup(h.Server.Close)
	return h
}

func (h *ModelHub) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/models/"+h.Repo+"/tree/main" {
		out := make([]map[string]any, 0, len(h.files))
		for name, data := range h.files {
			out = append(out, map[string]any{"type": "file", "path": name, "size": len(data)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/models/"+h.Repo+"/tree/") {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == "/api/v1/models/"+h.Repo+"/repo/files" {
		if raw := r.URL.Query().Get("Recursive"); raw != "" {
			if _, err := strconv.ParseBool(raw); err != nil {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"Code": 10010202002, "Message": "参数错误，信息：" + err.Error(), "Success": false})
				return
			}
		}
		requested, hasRevision := r.URL.Query()["Revision"]
		// A resolved default revision is a pinned commit. ModelScope serves the
		// same repository listing for both the default branch and that commit.
		if hasRevision && (len(requested) != 1 || (requested[0] != h.ModelScopeDefaultRef && requested[0] != h.CommitSHA)) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"Code": 200, "Data": map[string]any{"Files": nil}, "Success": true})
			return
		}
		items := make([]map[string]any, 0, len(h.files))
		for name, data := range h.files {
			items = append(items, map[string]any{"Path": name, "Name": name, "Size": len(data), "Type": "blob", "Revision": h.CommitSHA, "CommittedDate": int64(1787357763)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"Code": 200, "Data": map[string]any{"LatestCommitter": map[string]any{"CommittedDate": int64(1787357763), "ShortId": h.CommitSHA[:8]}, "Files": items}, "Success": true})
		return
	}
	if r.URL.Path == "/api/v1/models/"+h.Repo+"/repo" {
		file := r.URL.Query().Get("FilePath")
		if file == "" {
			http.NotFound(w, r)
			return
		}
		data, ok := h.files[file]
		if !ok {
			http.NotFound(w, r)
			return
		}
		serveRange(w, r, data)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	isModelScope := strings.HasPrefix(path, "models/")
	if isModelScope {
		path = strings.TrimPrefix(path, "models/")
	}
	marker := "/resolve/"
	at := strings.Index(path, marker)
	if at < 0 {
		http.NotFound(w, r)
		return
	}
	revisionAndFile := path[at+len(marker):]
	slash := strings.Index(revisionAndFile, "/")
	if slash < 0 {
		http.NotFound(w, r)
		return
	}
	revision := revisionAndFile[:slash]
	if isModelScope {
		// ModelScope's resolve endpoint tolerates the conventional "main" even
		// when its actual default branch has another name. The listing endpoint
		// above intentionally does not share this fallback.
		if revision != "main" && revision != h.ModelScopeDefaultRef && revision != h.CommitSHA {
			http.NotFound(w, r)
			return
		}
	} else if revision != "main" {
		http.NotFound(w, r)
		return
	}
	resolved := revisionAndFile[slash+1:]
	file, err := url.PathUnescape(resolved)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data, ok := h.files[file]
	if !ok {
		http.NotFound(w, r)
		return
	}
	serveRange(w, r, data)
}

func serveRange(w http.ResponseWriter, r *http.Request, data []byte) {
	w.Header().Set("Accept-Ranges", "bytes")
	start, end := 0, len(data)-1
	partial := false
	if raw := r.Header.Get("Range"); strings.HasPrefix(raw, "bytes=") {
		raw = strings.TrimPrefix(raw, "bytes=")
		parts := strings.SplitN(raw, "-", 2)
		if len(parts) != 2 {
			start, end = 0, len(data)-1
		} else if parts[0] == "" {
			// RFC 7233 suffix range: bytes=-N.
			n, err := strconv.Atoi(parts[1])
			if err != nil || n <= 0 {
				start, end = 0, len(data)-1
			} else {
				if n > len(data) {
					n = len(data)
				}
				start, end = len(data)-n, len(data)-1
			}
		} else {
			parsedStart, err := strconv.Atoi(parts[0])
			if err != nil || parsedStart < 0 {
				start = 0
			} else {
				start = parsedStart
			}
			if parts[1] == "" {
				end = len(data) - 1
			} else if parsedEnd, err := strconv.Atoi(parts[1]); err == nil {
				end = parsedEnd
			} else {
				end = len(data) - 1
			}
		}
		if start < 0 {
			start = 0
		}
		if start >= len(data) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if end >= len(data) || end < start {
			end = len(data) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		partial = true
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
	if partial {
		w.WriteHeader(http.StatusPartialContent)
	}
	if r.Method != http.MethodHead {
		_, _ = w.Write(data[start : end+1])
	}
}
