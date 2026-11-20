package seed

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"testing"
)

// A recipe must not silently move to an image that its source overlay rejects.
func TestGLM53SourceOverlaysPinTheirRecipeImage(t *testing.T) {
	for _, name := range []string{"w8a8-vllm-910c-dflash5-16npu", "w8a8-vllm-910b-graph-tp8"} {
		t.Run(name, func(t *testing.T) { checkGLM53OverlayPinsImage(t, name) })
	}
}

func checkGLM53OverlayPinsImage(t *testing.T, name string) {
	_, spec := catalogSpec(t, "zai-org/glm-5.3-flash", name)
	data, err := embeddedCatalog.ReadFile("catalog/models/zai-org/glm-5.3-flash/" + spec.Assets[0].File)
	if err != nil {
		t.Fatal(err)
	}
	zipped, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer zipped.Close()
	archive := tar.NewReader(zipped)
	files := map[string][]byte{}
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = content
	}
	var manifest struct {
		Image string `json:"image"`
		Files []struct {
			Path string `json:"path"`
			SHA  string `json:"sha256"`
		} `json:"files"`
	}
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Image != spec.Image || len(manifest.Files) == 0 {
		t.Fatalf("asset image %q does not match recipe %q", manifest.Image, spec.Image)
	}
	for _, file := range manifest.Files {
		payload, ok := files["payload/"+file.Path]
		hash := sha256.Sum256(payload)
		if !ok || hex.EncodeToString(hash[:]) != file.SHA {
			t.Errorf("invalid payload for %s", file.Path)
		}
	}
}
