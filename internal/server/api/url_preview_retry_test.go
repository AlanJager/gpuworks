package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/test/ocifixture"
)

func TestURLPreviewsRecoverFromTransientUpstream(t *testing.T) {
	for _, kind := range []string{"model listing", "model config", "image head", "image range"} {
		t.Run(kind, func(t *testing.T) {
			var failures atomic.Int32
			files := &urlDirectoryFixture{root: "/model", files: map[string]string{"config.json": urlModelConfig, "model.safetensors": "weights"}, sizes: map[string]string{}}
			archive, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{RepoTags: []string{"example/runner:v1"}, LayerBytes: 256 << 10})
			if err != nil {
				t.Fatal(err)
			}
			_, archiveServer := ocifixture.StartArchiveServer(archive)
			defer archiveServer.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				affected := (kind == "model listing" && r.URL.Path == "/model/") || (kind == "model config" && r.URL.Path == "/model/config.json") || (kind == "image head" && r.Method == http.MethodHead) || (kind == "image range" && r.Header.Get("Range") != "")
				if affected && failures.Add(1) == 1 {
					http.Error(w, "mirror temporarily busy", http.StatusServiceUnavailable)
					return
				}
				if kind == "model listing" || kind == "model config" {
					files.ServeHTTP(w, r)
					return
				}
				archiveServer.Config.Handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			h := &apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}}}
			handler, path, url := h.previewURLModel, "/api/v1/models/preview-url", server.URL+"/model"
			if kind == "image head" || kind == "image range" {
				handler, path, url = h.previewImageArchiveURL, "/api/v1/images/preview-archive-url", server.URL+"/image.tar"
			}
			rec := postJSON(t, handler, path, fmt.Sprintf(`{"url":%q}`, url), nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("temporary upstream should recover: status=%d body=%s", rec.Code, rec.Body.String())
			}
			if failures.Load() != 2 {
				t.Fatalf("affected requests=%d, want original plus one retry", failures.Load())
			}
		})
	}
}

func TestURLPreviewsDoNotRetryPermanentErrors(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			for _, kind := range []string{"model", "image"} {
				var hits atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					w.WriteHeader(status)
				}))
				h := &apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}}}
				handler, path, url := h.previewURLModel, "/api/v1/models/preview-url", server.URL+"/model"
				if kind == "image" {
					handler, path, url = h.previewImageArchiveURL, "/api/v1/images/preview-archive-url", server.URL+"/image.tar"
				}
				rec := postJSON(t, handler, path, fmt.Sprintf(`{"url":%q}`, url), nil)
				server.Close()
				if rec.Code != http.StatusBadGateway || hits.Load() != 1 {
					t.Fatalf("%s status=%d requests=%d", kind, rec.Code, hits.Load())
				}
			}
		})
	}
}

func TestRegisterURLModelRecoversBeforePersisting(t *testing.T) {
	var hits atomic.Int32
	files := &urlDirectoryFixture{root: "/model", files: map[string]string{"config.json": urlModelConfig, "model.safetensors": "weights"}, sizes: map[string]string{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/model/config.json" && hits.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		files.ServeHTTP(w, r)
	}))
	defer server.Close()
	st := urlModelStore(t, "register_url_transient")
	h := &apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st}}
	rec := postJSON(t, h.registerURLModel, "/api/v1/models/register-url", fmt.Sprintf(`{"source_url":%q}`, server.URL+"/model"), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"source_url":"`+server.URL+`/model"`) {
		t.Fatalf("wrong registered source: %s", rec.Body.String())
	}
}

// Exercise the reported ten-second boundary with real HTTP, including body
// reads. The timer models upstream latency; it is not a synchronization sleep.
func TestURLPreviewsAcceptMetadataSlowerThanTenSeconds(t *testing.T) {
	for _, kind := range []string{"model", "image"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			files := &urlDirectoryFixture{root: "/model", files: map[string]string{"config.json": urlModelConfig, "model.safetensors": "weights"}, sizes: map[string]string{}}
			archive, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{RepoTags: []string{"example/runner:v1"}, LayerBytes: 256 << 10})
			if err != nil {
				t.Fatal(err)
			}
			_, archiveServer := ocifixture.StartArchiveServer(archive)
			defer archiveServer.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if (kind == "model" && r.URL.Path == "/model/config.json") || (kind == "image" && r.Method == http.MethodHead) {
					timer := time.NewTimer(11 * time.Second)
					defer timer.Stop()
					select {
					case <-timer.C:
					case <-r.Context().Done():
						return
					}
				}
				if kind == "model" {
					files.ServeHTTP(w, r)
				} else {
					archiveServer.Config.Handler.ServeHTTP(w, r)
				}
			}))
			defer server.Close()
			h := &apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}}}
			handler, path, url := h.previewURLModel, "/api/v1/models/preview-url", server.URL+"/model"
			if kind == "image" {
				handler, path, url = h.previewImageArchiveURL, "/api/v1/images/preview-archive-url", server.URL+"/image.tar"
			}
			rec := postJSON(t, handler, path, fmt.Sprintf(`{"url":%q}`, url), nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("slow metadata status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
