package testenv

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/download"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

func TestFakeModelHubMetadataAndRangeDownload(t *testing.T) {
	h := FakeModelHub(t)
	meta, err := modelmeta.Fetch(context.Background(), modelrepo.SourceHF, h.Repo, h.Revision, "", h.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if meta.WeightBytes != h.WeightBytes {
		t.Fatalf("weight bytes = %d, want declared %d", meta.WeightBytes, h.WeightBytes)
	}
	if meta.Config == nil || meta.Config.ModelType != "qwen3" {
		t.Fatalf("unexpected config: %#v", meta.Config)
	}
	dest := filepath.Join(t.TempDir(), "model-00001-of-00003.safetensors")
	var progress int
	err = download.Download(context.Background(), download.Options{URL: modelrepo.ResolveURL(modelrepo.SourceHF, h.Repo, h.Revision, "model-00001-of-00003.safetensors", h.Endpoint), Dest: dest, Concurrency: 4}, func(download.Progress) { progress++ })
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 8<<20 {
		t.Fatalf("downloaded bytes = %d, want 8 MiB", st.Size())
	}
	if progress == 0 {
		t.Fatal("download did not report progress")
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatalf("partial file remains: %v", err)
	}
}

func TestFakeModelHubAlsoServesModelScopeShape(t *testing.T) {
	h := FakeModelHub(t)
	meta, err := modelmeta.Fetch(context.Background(), modelrepo.SourceMS, h.Repo, "", "", h.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if meta.WeightBytes != h.WeightBytes || meta.Config == nil || meta.Config.ModelType != "qwen3" {
		t.Fatalf("modelscope metadata=%+v", meta)
	}
	files, err := modelrepo.ListFiles(context.Background(), modelrepo.SourceMS, h.Repo, "", h.Endpoint, nil)
	if err != nil || len(files) == 0 || files[0].Revision != h.CommitSHA {
		t.Fatalf("modelscope default files=%+v err=%v", files, err)
	}
	if _, err := modelrepo.ListFiles(context.Background(), modelrepo.SourceMS, h.Repo, "main", h.Endpoint, nil); err == nil {
		t.Fatal("ModelScope wrong revision main unexpectedly returned files")
	}
	// Registration metadata uses resolve URLs and therefore reproduces the real
	// asymmetry: main resolves even though the listing API rejects it.
	meta, err = modelmeta.Fetch(context.Background(), modelrepo.SourceMS, h.Repo, "main", "", h.Endpoint)
	if err != nil || meta.Config == nil {
		t.Fatalf("modelscope resolve/main metadata=%+v err=%v", meta, err)
	}
}

func TestFakeModelHubRejectsNonMainHuggingFaceTree(t *testing.T) {
	h := FakeModelHub(t)
	if _, err := modelrepo.ListFiles(context.Background(), modelrepo.SourceHF, h.Repo, "master", h.Endpoint, nil); err == nil {
		t.Fatal("HuggingFace fake accepted tree/master")
	}
}

func TestFakeModelHubSupportsSuffixRanges(t *testing.T) {
	h := FakeModelHub(t)
	url := modelrepo.ResolveURL(modelrepo.SourceHF, h.Repo, h.Revision, "model-00001-of-00003.safetensors", h.Endpoint)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=-17")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || len(body) != 17 || resp.Header.Get("Content-Range") == "" {
		t.Fatalf("suffix range status=%d len=%d content-range=%q", resp.StatusCode, len(body), resp.Header.Get("Content-Range"))
	}
}
