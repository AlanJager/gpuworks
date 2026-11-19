package seed

import (
	"strings"
	"testing"
)

// imageNamesAVersion reports whether a container image reference carries a
// tag or a digest. A reference with neither is pulled as ":latest", which a
// registry need not have.
func imageNamesAVersion(ref string) bool {
	if strings.Contains(ref, "@") {
		return true
	}
	// A colon before the last slash is a registry port, not a tag.
	return strings.Contains(ref[strings.LastIndex(ref, "/")+1:], ":")
}

// Three Hygon vision specs once named image.sourcefind.cn:5000/dcu/admin/
// base/pytorch with neither tag nor digest. The runtime pulled ":latest",
// the repository has no such tag, and every deployment of them failed at
// the image pull (GWKS-374). The catalog does not require a digest, but a
// spec has to say which image it means.
func TestEveryEmbeddedSpecImageNamesATagOrDigest(t *testing.T) {
	for ref, want := range map[string]bool{
		"image.sourcefind.cn:5000/dcu/admin/base/pytorch": false,
		"vllm/vllm-openai": false,
		"image.sourcefind.cn:5000/dcu/admin/base/pytorch:2.5.1":       true,
		"image.sourcefind.cn:5000/dcu/admin/base/pytorch@sha256:52f2": true,
		"gpuworks/gpuworks-runner:dtk26.04-vllm0.18.1-r2":             true,
	} {
		if got := imageNamesAVersion(ref); got != want {
			t.Errorf("imageNamesAVersion(%q) = %v, want %v", ref, got, want)
		}
	}
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			for _, ref := range append([]string{spec.Image}, spec.ImageSources...) {
				if !imageNamesAVersion(strings.TrimSpace(ref)) {
					t.Errorf("%s/%s: image %q has neither a tag nor a digest and would be pulled as :latest", model.ModelID, spec.Name, ref)
				}
			}
		}
	}
}

// The Hygon spec of Qwen3-TTS VoiceDesign launched `vllm-omni` on an image
// that has no such program, so every start exited 127 (GWKS-374). The catalog
// has no Hygon image that provides it, and a spec that cannot start is not
// offered: the model keeps its other vendors' specs and has none for Hygon.
// A Hygon spec comes back with an image that carries its entrypoint; this
// case is then the place to say which.
func TestQwen3TTSVoiceDesignOffersNoHygonSpec(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	const modelID = "Qwen/Qwen3-TTS-12Hz-1.7B-VoiceDesign"
	vendors := map[string]int{}
	for _, model := range catalog.Models {
		if !strings.EqualFold(model.ModelID, modelID) {
			continue
		}
		for _, spec := range model.Specs {
			vendors[strings.ToLower(strings.TrimSpace(spec.GPUs.Vendor))]++
		}
	}
	if len(vendors) == 0 {
		t.Fatalf("%s is not in the embedded catalog", modelID)
	}
	if vendors["hygon"] != 0 || vendors["nvidia"] == 0 || vendors["ppu"] == 0 {
		t.Fatalf("%s specs by vendor = %v, want NVIDIA and PPU specs and none for Hygon", modelID, vendors)
	}
}
