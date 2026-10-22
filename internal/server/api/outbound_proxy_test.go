package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestArchivePreviewUsesTheManagementProxy(t *testing.T) {
	origin, size := archiveFixture(t, []string{"example/proxied:v1"})
	var hits atomic.Int64
	proxy := newTunnelingProxy(t, origin.URL, func() { hits.Add(1) })
	defer proxy.Close()
	st, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := &apiHandlers{d: Deps{Store: st}}
	for i := 0; i < 2; i++ {
		useManagementProxy(t, st, proxy.URL)
		response := postJSON(t, h.previewImageArchiveURL, "/api/v1/images/preview-archive-url", fmt.Sprintf(`{"url":%q}`, viaProxy(origin.URL)+"/image.tar"), nil)
		if response.Code != http.StatusOK {
			t.Fatalf("preview through configured proxy: status=%d body=%s", response.Code, response.Body.String())
		}
		var got dto.ImageArchivePreview
		if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.SizeBytes != size || len(got.RepoTags) != 1 || got.RepoTags[0] != "example/proxied:v1" {
			t.Fatalf("preview = %+v", got)
		}
		storeBrokenManagementProxy(t, st)
		response = postJSON(t, h.previewImageArchiveURL, "/api/v1/images/preview-archive-url", fmt.Sprintf(`{"url":%q}`, viaProxy(origin.URL)+"/image.tar"), nil)
		if response.Code != http.StatusBadGateway {
			t.Fatalf("invalid proxy must fail closed: %d %s", response.Code, response.Body.String())
		}
	}
	if hits.Load() == 0 {
		t.Fatal("proxy received no archive requests")
	}
}

func TestURLModelPreviewUsesTheManagementProxy(t *testing.T) {
	fixture := &urlDirectoryFixture{root: "/model", files: map[string]string{"config.json": `{"model_type":"qwen3"}`, "model.safetensors": "weights"}, sizes: map[string]string{"model.safetensors": "1K"}}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.ServeHTTP(w, r)
	}))
	defer origin.Close()
	proxy := newTunnelingProxy(t, origin.URL, nil)
	defer proxy.Close()
	st, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	useManagementProxy(t, st, proxy.URL)
	h := &apiHandlers{d: Deps{Store: st}}
	response := postJSON(t, h.previewURLModel, "/api/v1/models/preview-url", fmt.Sprintf(`{"url":%q}`, viaProxy(origin.URL)+"/model"), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", response.Code, response.Body.String())
	}
	var got urlModelPreview
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Arch != "qwen3" || got.WeightBytes != 7 || got.FilesCount != 2 {
		t.Fatalf("preview = %+v", got)
	}
	response = postJSON(t, h.registerURLModel, "/api/v1/models/register-url", fmt.Sprintf(`{"source_url":%q}`, viaProxy(origin.URL)+"/model"), nil)
	if response.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", response.Code, response.Body.String())
	}
	var registered dto.Model
	if err := json.Unmarshal(response.Body.Bytes(), &registered); err != nil {
		t.Fatal(err)
	}
	if registered.Arch != "qwen3" || registered.WeightGiB <= 0 {
		t.Fatalf("registered metadata = %+v", registered)
	}

}
