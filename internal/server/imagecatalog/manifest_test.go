package imagecatalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/imagecatalog/manifest"
)

func TestManifestValidateAndVerify(t *testing.T) {
	m, err := manifest.Parse([]byte(`{"schema_version":2,"catalog_version":"v3","series":[{"name":"vLLM","engine":"vllm","framework":"cuda","versions":[{"image_ref":"gpustack/runner:cuda12-vllm1"}]}]}`))
	if err != nil || m.CatalogVersion != "v3" {
		t.Fatalf("manifest=%+v err=%v", m, err)
	}
	if _, err := manifest.Parse([]byte(`{"schema_version":2,"catalog_version":"v3","series":[{"name":"x","versions":[{"image_ref":"x/y@sha256:abc"}]}]}`)); err == nil {
		t.Fatal("digest ref should be rejected")
	}
}

func TestManifestHashVerification(t *testing.T) {
	m := manifest.Manifest{
		SchemaVersion:  2,
		CatalogVersion: "v3",
		Series:         []manifest.ManifestSeries{{Name: "x", Versions: []manifest.ManifestVersion{{ImageRef: "x/y:v3"}}}},
	}
	canonical, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	m.SHA256 = hex.EncodeToString(sum[:])
	raw, _ := json.Marshal(m)
	if parsed, err := manifest.Parse(raw); err != nil || parsed.SHA256 == "" {
		t.Fatalf("hashed manifest=%+v err=%v", parsed, err)
	}
	m.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	bad, _ := json.Marshal(m)
	if _, err := manifest.Parse(bad); err == nil {
		t.Fatal("tampered manifest hash was accepted")
	}
}
