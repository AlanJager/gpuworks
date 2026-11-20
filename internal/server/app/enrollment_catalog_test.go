//go:build linux && dqlite

package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"gpuworks.ai/gpuworks/internal/server/enrollment"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/catalogasset"
)

// Uses the real enrollment listener, CA, admission and TLS clients from the
// material-channel fixture, including its later revoke/grant-failure checks.
func checkEnrollmentCatalogTransfer(t *testing.T, st *store.Store, catalog *packages.Store, root string, client *enrollment.Client, cert, key []byte, request func(*http.Client, string, string, string, int) []byte, anonymous, admitted, unknown *http.Client) {
	t.Helper()
	base := enrollment.CatalogPackagesPath
	for _, path := range []string{base, base + "/" + strings.Repeat("a", 64)} {
		request(anonymous, "POST", path, "", 401)
		request(unknown, "POST", path, "", 403)
		request(admitted, "GET", path, "", 405)
		request(admitted, "POST", path+"?token=x", "", 400)
		request(admitted, "POST", path, "{}", 400)
	}
	empty, err := client.FetchCatalogIndex(t.Context(), cert, key)
	if err != nil || empty.Digests == nil || len(empty.Digests) != 0 {
		t.Fatalf("empty index: %+v %v", empty, err)
	}
	request(admitted, "POST", base+"/bad", "", 400)
	request(admitted, "POST", base+"/"+strings.Repeat("a", 64), "", 404)
	model := inferencecatalog.InferenceModel{ModelID: "org/model", DisplayTitle: "Model", Specs: []inferencecatalog.InferenceSpec{{Name: "default", Engine: "vllm", Image: "vllm/vllm-openai:latest", Weights: inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: "org/model"}}}, GPUs: inferencecatalog.GPUSelector{Vendor: "nvidia", CC: ">=8.0"}}}}
	yaml, err := inferencecatalog.MarshalModelYAML(model)
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{"manifest.yaml": {Data: []byte("schema_version: 1\npublished_at: 2026-09-21T00:00:00Z\nmodels: 1\nspecs: 1\n")}, "models/org/model/model.yaml": {Data: yaml}, "models/org/model/README.en.md": {Data: []byte("source package")}}
	digest, _, err := catalog.Put(fsys)
	if err != nil {
		t.Fatal(err)
	}
	// Existence on disk does not grant export permission.
	request(admitted, "POST", base+"/"+digest, "", 404)
	row := store.InferenceModel{ModelID: "org/model", Source: "user", PackageDigest: digest}
	if err := st.DB().Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	raw := request(admitted, "POST", base, "", 200)
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil || len(wire) != 3 || wire["cluster_id"] == nil || wire["protocol"] == nil || wire["digests"] == nil {
		t.Fatal("index wire contract", string(raw), err)
	}
	index, err := client.FetchCatalogIndex(t.Context(), cert, key)
	if err != nil || len(index.Digests) != 1 || index.Digests[0] != digest {
		t.Fatal("reference transfer", index, err)
	}
	archive, err := client.FetchCatalogPackage(t.Context(), cert, key, digest)
	if err != nil {
		t.Fatal(err)
	}
	target, err := packages.New(filepath.Join(t.TempDir(), "target"))
	if err != nil {
		t.Fatal(err)
	}
	got, created, err := target.Put(archive.FS)
	if err != nil || !created || got != digest {
		t.Fatal("persist received archive", got, created, err)
	}
	for member, copy := range map[string]*packages.Store{"source": catalog, "receiver": target} {
		asset, err := copy.ReadDocumentation(digest, "org/model", "README.en.md")
		if err != nil || string(asset) != "source package" {
			t.Fatalf("%s does not possess readable package bytes: %q %v", member, asset, err)
		}
	}
	// A missing or corrupt source must fail before sending a successful archive.
	doc := filepath.Join(root, digest, "models/org/model/README.en.md")
	if err := os.WriteFile(doc, []byte("tampered"), 0644); err != nil {
		t.Fatal(err)
	}
	request(admitted, "POST", base+"/"+digest, "", 503)
	if got, err := client.FetchCatalogPackage(t.Context(), cert, key, digest); err == nil || got.FS != nil {
		t.Fatal("accepted corrupt source")
	}
	if err := os.RemoveAll(filepath.Join(root, digest)); err != nil {
		t.Fatal(err)
	}
	request(admitted, "POST", base+"/"+digest, "", 503)
	if asset, err := target.ReadDocumentation(digest, "org/model", "README.en.md"); err != nil || string(asset) != "source package" {
		t.Fatalf("receiver lost bytes when source member failed: %q %v", asset, err)
	}
	// Malformed references must not produce a partial index.
	if err := st.DB().Model(&row).Update("package_digest", "private-invalid").Error; err != nil {
		t.Fatal(err)
	}
	raw = request(admitted, "POST", base, "", 503)
	if bytes.Contains(raw, []byte("private-invalid")) {
		t.Fatal("reference data leaked")
	}
	if err := st.DB().Model(&row).Update("package_digest", digest).Error; err != nil {
		t.Fatal(err)
	}
}

func TestCatalogTransferBufferIsBounded(t *testing.T) {
	var buf catalogTransferBuffer
	if n, err := buf.Write(make([]byte, catalogasset.MaxPackageBytes)); err != nil || int64(n) != catalogasset.MaxPackageBytes {
		t.Fatal(n, err)
	}
	if n, err := buf.Write([]byte{1}); err == nil || n != 0 || int64(buf.Len()) != catalogasset.MaxPackageBytes {
		t.Fatal("buffer limit bypassed")
	}
}
