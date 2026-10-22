package modelmeta

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// quantRepo is the layout of ISTA-DASLab/Qwen3.8-Flash-Next-GSQ-RCO-GGUF as
// both hubs list it: several GGUF quantizations side by side in one repository,
// a multimodal projector at the root, and no config.json.
var quantRepo = []struct {
	path string
	size int64
}{
	{".gitattributes", 2075},
	{"IQ2_XS/Qwen3.8-Flash-Next-GSQ-RCO-IQ2_XS-00001-of-00002.gguf", 39225954592},
	{"IQ2_XS/Qwen3.8-Flash-Next-GSQ-RCO-IQ2_XS-00002-of-00002.gguf", 28800138432},
	{"IQ2_XS_big/Qwen3.8-Flash-Next-GSQ-RCO-IQ2_XS_big-00001-of-00001.gguf", 70000000000},
	{"IQ3_S/Qwen3.8-Flash-Next-GSQ-RCO-IQ3_S-00001-of-00002.gguf", 54817524224},
	{"IQ3_S/Qwen3.8-Flash-Next-GSQ-RCO-IQ3_S-00002-of-00002.gguf", 28800138432},
	{"Q2_0/Qwen3.8-Flash-Next-GSQ-RCO-Q2_0-00001-of-00002.gguf", 37623740192},
	{"Q2_0/Qwen3.8-Flash-Next-GSQ-RCO-Q2_0-00002-of-00002.gguf", 28800138432},
	{"README.md", 15693},
	{"assets/banner.png", 309501},
	{"mmproj-Qwen3.8-Flash-Next-BF16.gguf", 907543008},
}

const (
	iq2xsBytes = int64(39225954592 + 28800138432)
	wholeRepo  = int64(39225954592+28800138432+70000000000+54817524224+28800138432+37623740192+28800138432) + 907543008
)

// quantHub serves quantRepo the way Hugging Face and ModelScope do: a
// recursive listing that includes directory entries, and 404 for any file the
// repository does not hold (there is no config.json).
func quantHub(t *testing.T, extra map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/acme/quants":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"author":"acme","createdAt":"2025-01-02T03:04:05Z","lastModified":"2025-02-03T04:05:06Z"}`))
		case "/api/v1/models/acme/quants":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Code":200,"Data":{"Avatar":"","CreatedTime":1746153600,"LastUpdatedTime":1754006400}}`))
		case "/api/models/acme/quants/tree/main":
			if r.URL.Query().Get("recursive") != "true" {
				http.Error(w, `{"error":"tree is not recursive"}`, http.StatusBadRequest)
				return
			}
			items := []map[string]any{
				{"type": "directory", "oid": "a2d97c5b", "size": 0, "path": "IQ2_XS"},
				{"type": "directory", "oid": "b3e08d6c", "size": 0, "path": "IQ3_S"},
			}
			for _, f := range quantRepo {
				items = append(items, map[string]any{"type": "file", "oid": "c4f19e7d", "size": f.size, "path": f.path})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(items)
		case "/api/v1/models/acme/quants/repo/files":
			if r.URL.Query().Get("Recursive") != "true" {
				http.Error(w, `{"Code":400,"Message":"bad request"}`, http.StatusBadRequest)
				return
			}
			files := []map[string]any{
				{"Type": "tree", "Name": "IQ2_XS", "Path": "IQ2_XS", "Size": 0, "Revision": "f1853d0f9b8146bb2551b7d379ea333587be76cc", "CommittedDate": 1789499531},
			}
			for _, f := range quantRepo {
				files = append(files, map[string]any{"Type": "blob", "Name": f.path[strings.LastIndex(f.path, "/")+1:], "Path": f.path, "Size": f.size, "Revision": "1868010e27505d4b8cc15970cdcc4dd76005d7de", "CommittedDate": 1789499531})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"Code": 200, "Success": true, "Data": map[string]any{
				"LatestCommitter": map[string]any{"CommittedDate": 1789499531, "ShortId": "1868010e"}, "Files": files}})
		default:
			for path, body := range extra {
				if r.URL.Path == path {
					_, _ = w.Write([]byte(body))
					return
				}
			}
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchSizesASubfolderModelByItsOwnWeightFiles(t *testing.T) {
	srv := quantHub(t, nil)
	for _, source := range []string{"huggingface", "modelscope"} {
		t.Run(source, func(t *testing.T) {
			revision := "main"
			if source == "modelscope" {
				revision = "1868010e27505d4b8cc15970cdcc4dd76005d7de"
			}
			scoped, err := Fetch(context.Background(), source, "acme/quants", revision, "IQ2_XS", srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			if scoped.WeightBytes != iq2xsBytes {
				t.Fatalf("subfolder weight bytes = %d, want %d (the two IQ2_XS shards only)", scoped.WeightBytes, iq2xsBytes)
			}
			whole, err := Fetch(context.Background(), source, "acme/quants", revision, "", srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			if whole.WeightBytes != wholeRepo {
				t.Fatalf("whole repository weight bytes = %d, want %d", whole.WeightBytes, wholeRepo)
			}
		})
	}
}

func TestFetchSubfolderDoesNotUseTheRepositoryRootIndex(t *testing.T) {
	// The root index sizes the full-precision checkpoint at the repository
	// root; a quantization in a subfolder must neither inherit it nor lose its
	// own listing because of it.
	srv := quantHub(t, map[string]string{
		"/acme/quants/resolve/main/model.safetensors.index.json": `{"metadata":{"total_size":999999999999}}`,
	})
	meta, err := Fetch(context.Background(), "huggingface", "acme/quants", "main", "IQ2_XS", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if meta.WeightBytes != iq2xsBytes {
		t.Fatalf("weight bytes = %d, want %d", meta.WeightBytes, iq2xsBytes)
	}
}

func TestFetchSubfolderReadsItsOwnShardIndex(t *testing.T) {
	srv := quantHub(t, map[string]string{
		"/acme/quants/resolve/main/IQ2_XS/model.safetensors.index.json": `{"metadata":{"total_size":4242}}`,
	})
	meta, err := Fetch(context.Background(), "huggingface", "acme/quants", "main", "IQ2_XS", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if meta.WeightBytes != 4242 {
		t.Fatalf("weight bytes = %d, want the subfolder index total 4242", meta.WeightBytes)
	}
}

func TestFetchSubfolderMatchesWholeDirectoriesOnly(t *testing.T) {
	srv := quantHub(t, nil)
	// IQ2_XS_big shares IQ2_XS as a name prefix but is a different directory.
	meta, err := Fetch(context.Background(), "huggingface", "acme/quants", "main", "IQ2_XS_big", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if meta.WeightBytes != 70000000000 {
		t.Fatalf("weight bytes = %d, want 70000000000", meta.WeightBytes)
	}
}

func TestFetchUnknownSubfolderLeavesTheWeightSizeUnknown(t *testing.T) {
	srv := quantHub(t, nil)
	for _, source := range []string{"huggingface", "modelscope"} {
		meta, err := Fetch(context.Background(), source, "acme/quants", "main", "Q9_9", srv.URL)
		if err != nil {
			t.Fatalf("%s: %v", source, err)
		}
		if meta.WeightBytes != 0 {
			t.Fatalf("%s: weight bytes = %d, want 0 rather than the whole repository", source, meta.WeightBytes)
		}
	}
}

func TestFetchRejectsAnUnsafeSubfolder(t *testing.T) {
	srv := quantHub(t, nil)
	for _, subfolder := range []string{"../etc", "/IQ2_XS", "IQ2_XS/", `IQ2_XS\x`} {
		if _, err := Fetch(context.Background(), "huggingface", "acme/quants", "main", subfolder, srv.URL); err == nil {
			t.Fatalf("subfolder %q was accepted", subfolder)
		}
	}
}
