package seed

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

func TestQwen3PPUWeightSourcesUseProviderScopedRevisions(t *testing.T) {
	_, spec := ppuSpec(t, "qwen/qwen3-0.6b", "bf16-vllm-ppu-c1899de2")
	const hfRevision = "c1899de289a04d12100db370d81485cdf75e47ca"
	const msRevision = "09b42cad3d112e832108974449ccb5e8e0f5b5d1"
	// The public ModelScope endpoint returns HTTP 404 / Code 10990101004 for
	// this HuggingFace commit, but lists the repository for its own commit.
	// Keep the regression offline and exercise the real repository client.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/models/Qwen/Qwen3-0.6B/repo/files" || r.URL.Query().Get("Recursive") != "true" {
			t.Errorf("unexpected repository request: %s %s", r.Method, r.URL)
			http.Error(w, `{"Code":400,"Message":"invalid request","Success":false}`, http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("Revision") != msRevision {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"Code":10990101004,"Message":"failed to get latest commit: 404 Not Found","Success":false}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"Code":200,"Message":"success","Success":true,"Data":{"Files":[{"Path":"config.json","Size":1024,"Type":"blob","Revision":%q}]}}`, msRevision)
	}))
	defer server.Close()
	seenHF, seenMS := false, false
	for _, source := range spec.Weights.Sources {
		switch source.Provider {
		case "huggingface":
			seenHF = true
			if source.Revision != hfRevision {
				t.Fatalf("HuggingFace pin changed: %s", source.Revision)
			}
		case "modelscope":
			seenMS = true
			files, err := modelrepo.ListFiles(context.Background(), modelrepo.SourceMS, source.RepoID, source.Revision, server.URL, nil)
			if err != nil || len(files) != 1 || files[0].Revision != msRevision {
				t.Fatalf("catalog ModelScope revision cannot list its repository: files=%+v err=%v", files, err)
			}
		}
	}
	if !seenHF || !seenMS {
		t.Fatal("both explicitly selectable providers must remain available")
	}
}
