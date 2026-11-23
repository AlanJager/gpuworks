package app

import (
	"os"
	"path/filepath"
	"testing"
)

// A failed download reports the bytes it has, not the length of the file it
// preallocated or of its resume bookkeeping.
func TestModelRepoBytesDoneCountsAnUnfinishedFileByItsWrittenBytes(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("config.json", make([]byte, 21))
	write("tokenizer.json.part", make([]byte, 7)) // single connection: appended
	write("model.safetensors.part", make([]byte, 1000))
	write("model.safetensors.part.state", []byte(`{"url":"k","size":1000,"chunks":[100,250,750,800]}`))
	write("model.safetensors.part.state.tmp", []byte(`{"url":"k","size":1000,"chunks":[100,250,750,800]}`))
	if got, want := modelRepoBytesDone(dir), int64(21+7+400); got != want {
		t.Fatalf("bytes done = %d, want %d", got, want)
	}
}
