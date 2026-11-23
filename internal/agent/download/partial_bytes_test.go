package download

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPartialBytesCountsWhatWasWrittenNotThePreallocatedLength(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "model.safetensors")
	if got := PartialBytes(dest); got != 0 {
		t.Fatalf("no part file = %d bytes, want 0", got)
	}
	// A single-connection download appends: the length is the bytes.
	if err := os.WriteFile(dest+".part", make([]byte, 300), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := PartialBytes(dest); got != 300 {
		t.Fatalf("appended part = %d bytes, want 300", got)
	}
	// A multi-connection download preallocates all 1000 bytes; four chunks of
	// 250 hold 100, 0, 250 (complete) and 50.
	if err := os.Truncate(dest+".part", 1000); err != nil {
		t.Fatal(err)
	}
	writeState(dest+".part.state", downloadState{URL: "k", Size: 1000, Chunks: []int64{100, 250, 750, 800}})
	if got := PartialBytes(dest); got != 400 {
		t.Fatalf("preallocated part = %d bytes, want 400", got)
	}
	// A state that describes another file, or none at all, vouches for nothing.
	for name, state := range map[string]string{
		"another size": `{"url":"k","size":999,"chunks":[100,250,750,800]}`,
		"no chunks":    `{"url":"k","size":1000,"chunks":[]}`,
		"not json":     `{`,
	} {
		if err := os.WriteFile(dest+".part.state", []byte(state), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := PartialBytes(dest); got != 0 {
			t.Errorf("%s: %d bytes, want 0", name, got)
		}
	}
	// Offsets outside their chunk are clamped, as a resume clamps them.
	writeState(dest+".part.state", downloadState{URL: "k", Size: 1000, Chunks: []int64{-5, 9999, 500, 750}})
	if got := PartialBytes(dest); got != 250 {
		t.Fatalf("clamped offsets = %d bytes, want 250", got)
	}
}
