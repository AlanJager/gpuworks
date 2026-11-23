package modelfetch

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/download"
)

// fetchWeights fetches the weight file the way the agent does: four
// connections, under the branch name the model was added with.
func fetchWeights(t *testing.T, f *Fetcher, dir string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := f.List(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	return f.Fetch(ctx, File{Path: "model.safetensors", Revision: "main", Dest: filepath.Join(dir, "model.safetensors"), Concurrency: 4}, nil)
}

// A download that fails because its only address keeps dropping the
// connection leaves what it received on disk, and the retry asks for the rest.
func TestARetryAfterAFailedDownloadContinuesTheBytesOnDisk(t *testing.T) {
	const perAttempt = 1024 // well under a quarter of the file
	files := modelFiles()
	weights := files["model.safetensors"]
	source := newHub(t, files)
	source.cut = map[string]int{"model.safetensors": perAttempt}
	dir := t.TempDir()
	dest := filepath.Join(dir, "model.safetensors")

	if err := fetchWeights(t, newFetcher(t, &cooler{}, &run{}, source.URL), dir); err == nil {
		t.Fatal("a source that drops every transfer must fail the download")
	}
	// How much each connection received before a sibling's failure stopped it
	// varies; what matters is that it is still there.
	kept := download.PartialBytes(dest)
	if kept <= 0 || kept > 3*4*perAttempt {
		t.Fatalf("bytes kept after the failure = %d, want some of the %d the source sent", kept, 3*4*perAttempt)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("an unfinished file must not be installed: %v", err)
	}

	source.cut = nil
	before := len(source.startsOf("model.safetensors"))
	if err := fetchWeights(t, newFetcher(t, &cooler{}, &run{}, source.URL), dir); err != nil {
		t.Fatal(err)
	}
	assertFiles(t, dir, map[string][]byte{"model.safetensors": weights})
	starts := source.startsOf("model.safetensors")[before:]
	quarter := int64(len(weights) / 4)
	var skipped int64
	for _, start := range starts {
		skipped += start % quarter
	}
	if len(starts) != 4 || skipped != kept {
		t.Fatalf("the retry asked for bytes from %v, skipping %d; want four requests skipping the %d bytes on disk", starts, skipped, kept)
	}
	for _, leftover := range []string{dest + ".part", dest + ".part.state"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Fatalf("%s remains after the download finished: %v", filepath.Base(leftover), err)
		}
	}
}

// What one address left behind is not continued at another: under a branch
// name the two may serve different bytes.
func TestARetryAtAnotherAddressDoesNotContinueTheFailedOnesBytes(t *testing.T) {
	files := modelFiles()
	failed, other := newHub(t, files), newHub(t, files)
	failed.cut = map[string]int{"model.safetensors": 1024}
	dir := t.TempDir()
	if err := fetchWeights(t, newFetcher(t, &cooler{}, &run{}, failed.URL), dir); err == nil {
		t.Fatal("a source that drops every transfer must fail the download")
	}
	if err := fetchWeights(t, newFetcher(t, &cooler{}, &run{}, other.URL), dir); err != nil {
		t.Fatal(err)
	}
	assertFiles(t, dir, map[string][]byte{"model.safetensors": files["model.safetensors"]})
	starts := other.startsOf("model.safetensors")
	slices.Sort(starts)
	quarter := int64(len(files["model.safetensors"]) / 4)
	if want := []int64{0, quarter, 2 * quarter, 3 * quarter}; !slices.Equal(starts, want) {
		t.Fatalf("the other address was asked for bytes from %v, want %v", starts, want)
	}
}
