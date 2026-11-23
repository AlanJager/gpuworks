package seed

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/placement"
)

// repoListing is a provider's file listing of one repository revision.
type repoListing struct {
	Provider string `json:"provider"`
	RepoID   string `json:"repo_id"`
	Revision string `json:"revision"`
	Files    []struct {
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

// The Inkling 2 Mini spec loads two files out of a repository that keeps every
// quantization side by side, so its launch arguments and weight_bytes are only
// right against that repository's listing at the pinned revisions. The fixture
// is the listing both providers returned for those revisions (2026-11-22).
func TestInkling2MiniGGUFNamesFilesOfThePinnedRevisions(t *testing.T) {
	_, spec := catalogSpec(t, "thinkingmachines/inkling-2-mini-gguf", "iq4nl-llamacpp-apple")
	var listings []repoListing
	data, err := os.ReadFile("testdata/inkling-2-mini-gguf-listing.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &listings); err != nil {
		t.Fatal(err)
	}
	if len(listings) != len(spec.Weights.Sources) {
		t.Fatalf("fixture lists %d sources, the spec declares %d", len(listings), len(spec.Weights.Sources))
	}
	loaded := map[string]bool{}
	for index, arg := range spec.Args {
		if (arg == "-m" || arg == "--mmproj") && index+1 < len(spec.Args) {
			loaded[strings.TrimPrefix(spec.Args[index+1], "${GPUWORKS_MODEL_PATH}/")] = true
		}
	}
	if !loaded["Inkling-2-Mini-IQ4_NL.gguf"] || !loaded["mmproj-Inkling-2-Mini-Q8_0.gguf"] || len(loaded) != 2 {
		t.Fatalf("launch loads %v, want the IQ4_NL model and the Q8_0 vision projector", loaded)
	}
	var digests []map[string]string
	for _, source := range spec.Weights.Sources {
		index := slices.IndexFunc(listings, func(l repoListing) bool {
			return l.Provider == source.Provider && l.RepoID == source.RepoID && l.Revision == source.Revision
		})
		if index < 0 {
			t.Fatalf("no fixture listing for %s %s@%s", source.Provider, source.RepoID, source.Revision)
		}
		var loadedBytes int64
		found, digest := 0, map[string]string{}
		for _, file := range listings[index].Files {
			if loaded[file.Path] {
				loadedBytes += file.Size
				found++
				digest[file.Path] = file.SHA256
			}
		}
		if found != len(loaded) {
			t.Fatalf("%s@%s holds %d of the %d files the launch loads", source.Provider, source.Revision, found, len(loaded))
		}
		if loadedBytes != spec.WeightBytes {
			t.Errorf("%s: the loaded files are %d bytes, weight_bytes says %d", source.Provider, loadedBytes, spec.WeightBytes)
		}
		digests = append(digests, digest)
	}
	// The two revisions are different commits of one upload: the files the
	// engine loads must be byte-identical whichever provider served them.
	for _, digest := range digests[1:] {
		for path, sum := range digests[0] {
			if digest[path] != sum {
				t.Errorf("%s differs between providers: %s vs %s", path, sum, digest[path])
			}
		}
	}
	// Without --alias llama-server answers under its -m path, which on a Mac
	// is a host path; the deployment's served name is what clients see.
	alias := slices.Index(spec.Args, "--alias")
	if alias < 0 || alias+1 >= len(spec.Args) || spec.Args[alias+1] != "${GPUWORKS_SERVED_MODEL_NAME}" {
		t.Errorf("args %v do not name the model after the deployment (--alias ${GPUWORKS_SERVED_MODEL_NAME})", spec.Args)
	}
	// Upstream llama.cpp loads inkling2_loop from b12600 on; the b11617
	// package beside it cannot.
	if spec.Image != "gpuworks/gpuworks-runner:metal15.0-llama.cpp-b12631-r1" {
		t.Errorf("image = %s, want the b12631 llama.cpp package", spec.Image)
	}
}

func TestInkling2MiniGGUFAdmission(t *testing.T) {
	_, spec := catalogSpec(t, "thinkingmachines/inkling-2-mini-gguf", "iq4nl-llamacpp-apple")
	resolve := func(memoryGiB float64) placement.Result {
		t.Helper()
		result, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{
			Count: 1, Vendor: "apple", Names: []string{"Apple M4 Max"}, MemoriesGiB: []float64{memoryGiB}, MinMemoryGiB: memoryGiB,
		})
		if err != nil {
			t.Fatalf("memory=%v: %v", memoryGiB, err)
		}
		return result
	}
	// A 36 GB Mac has a 27 GiB working set by default (3/4 above 32 GiB).
	if result := resolve(27); result.Reason != "" || result.GPUMemoryMiBPerDevice != 24576 {
		t.Errorf("36 GB Mac: reason %q, claim %d MiB; want accepted with 24576", result.Reason, result.GPUMemoryMiBPerDevice)
	}
	// A 32 GB Mac has 21.3 GiB (2/3 at or below 32 GiB).
	if result := resolve(21.3); result.Reason == "" {
		t.Error("a 32 GB Mac is accepted")
	}
}
