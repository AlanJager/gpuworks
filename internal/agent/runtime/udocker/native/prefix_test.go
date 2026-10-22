package native

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGrowingFilePrefixTracksLeadingRunOnly(t *testing.T) {
	// 100 bytes in 4 parts of 25.
	g := newGrowingFile(100, 25, 4)
	if got, _ := g.waitFor(0); got != 0 {
		t.Fatalf("initial prefix = %d, want 0", got)
	}
	// Filling a later part must not advance the readable prefix: the bytes
	// before it have not arrived, so nothing there is contiguous.
	g.advance(2, 25)
	if got, _ := g.waitFor(0); got != 0 {
		t.Errorf("prefix after filling part 2 = %d, want 0", got)
	}
	g.advance(0, 10)
	if got, _ := g.waitFor(0); got != 10 {
		t.Errorf("prefix after 10 bytes of part 0 = %d, want 10", got)
	}
	g.advance(0, 15) // part 0 complete
	// Part 1 is empty, so the prefix stops at the end of part 0 even though
	// part 2 is full.
	if got, _ := g.waitFor(0); got != 25 {
		t.Errorf("prefix with part 1 missing = %d, want 25", got)
	}
	g.advance(1, 25)
	// Now parts 0,1,2 are contiguous.
	if got, _ := g.waitFor(0); got != 75 {
		t.Errorf("prefix with parts 0-2 full = %d, want 75", got)
	}
	g.advance(3, 25)
	if got, _ := g.waitFor(100); got != 100 {
		t.Errorf("final prefix = %d, want 100", got)
	}
}

// The last part is short whenever size is not a multiple of the chunk; the
// prefix must still reach exactly size rather than overshooting.
func TestGrowingFilePrefixHandlesShortFinalPart(t *testing.T) {
	g := newGrowingFile(90, 25, 4) // parts of 25,25,25,15
	g.advance(0, 25)
	g.advance(1, 25)
	g.advance(2, 25)
	g.advance(3, 15)
	if got, _ := g.waitFor(90); got != 90 {
		t.Errorf("prefix = %d, want 90", got)
	}
}

func TestGrowingFileWaitForReleasesOnFinish(t *testing.T) {
	g := newGrowingFile(100, 100, 1)
	done := make(chan int64, 1)
	go func() {
		prefix, err := g.waitFor(100)
		if err != nil {
			t.Error(err)
		}
		done <- prefix
	}()
	g.advance(0, 40)
	g.finish(nil) // completes without the full 100 bytes
	select {
	case prefix := <-done:
		if prefix != 40 {
			t.Errorf("prefix = %d, want 40", prefix)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitFor did not return after finish")
	}
}

func TestGrowingFileWaitForReportsFailure(t *testing.T) {
	g := newGrowingFile(100, 100, 1)
	sentinel := errors.New("connection reset")
	done := make(chan error, 1)
	go func() {
		_, err := g.waitFor(100)
		done <- err
	}()
	g.finish(sentinel)
	select {
	case err := <-done:
		if !errors.Is(err, sentinel) {
			t.Errorf("err = %v, want %v", err, sentinel)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitFor did not return after a failure")
	}
}

// A reader must see the whole payload even when it is written in pieces from
// several parts, out of order, while the reader is already consuming.
func TestPrefixReaderFollowsOutOfOrderWrites(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdefghij"), 40) // 400 bytes
	chunk := int64(100)
	path := filepath.Join(t.TempDir(), "blob")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.Truncate(int64(len(payload))); err != nil {
		t.Fatal(err)
	}

	g := newGrowingFile(int64(len(payload)), chunk, 4)
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	result := make(chan []byte, 1)
	go func() {
		data, err := io.ReadAll(newPrefixReader(reader, g))
		if err != nil {
			t.Error(err)
		}
		result <- data
	}()

	// Write the parts in reverse order, in two halves each, so the reader is
	// forced to wait on the leading part repeatedly.
	for _, part := range []int{3, 2, 1, 0} {
		start := int64(part) * chunk
		for _, half := range []int64{0, chunk / 2} {
			offset := start + half
			if _, err := file.WriteAt(payload[offset:offset+chunk/2], offset); err != nil {
				t.Fatal(err)
			}
			g.advance(part, chunk/2)
			time.Sleep(time.Millisecond)
		}
	}
	g.finish(nil)

	select {
	case data := <-result:
		if !bytes.Equal(data, payload) {
			t.Errorf("read %d bytes, want %d identical", len(data), len(payload))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prefixReader did not finish")
	}
}

// A transfer that ends early must surface as an unexpected EOF so the caller
// retries, rather than looking like a complete but truncated layer.
func TestPrefixReaderReportsTruncatedDownload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(path, make([]byte, 50), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	g := newGrowingFile(100, 100, 1)
	g.advance(0, 50)
	g.finish(nil)
	_, err = io.ReadAll(newPrefixReader(file, g))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}
