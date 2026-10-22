package native

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	kgzip "github.com/klauspost/compress/gzip"
)

// syntheticLayer builds a gzip layer that behaves like a real engine image
// layer: many small files, a few large ones, and content that compresses at a
// realistic ratio rather than to nothing (zeros would make inflate free and
// hide exactly the cost being measured).
func syntheticLayer(tb testing.TB, files int, totalBytes int64) []byte {
	tb.Helper()
	// A dictionary-ish corpus: compresses roughly 2-3x, like machine code and
	// Python sources do, so the inflate/write ratio resembles production.
	corpus := make([]byte, 1<<20)
	rng := rand.New(rand.NewSource(1))
	words := [][]byte{
		[]byte("import "), []byte("return "), []byte("def "), []byte("self."),
		[]byte("torch"), []byte("cuda"), []byte("0x00"), []byte("ELF"),
		[]byte("nvidia"), []byte("kernel"), []byte("__init__"), []byte("None"),
	}
	for offset := 0; offset < len(corpus); {
		word := words[rng.Intn(len(words))]
		offset += copy(corpus[offset:], word)
	}

	var raw bytes.Buffer
	raw.Grow(int(totalBytes) + files*1024)
	writer := tar.NewWriter(&raw)
	// Two thirds of the bytes go into a handful of big files, the rest spread
	// over many small ones -- the shape of a CUDA/python layer.
	bigFiles := 8
	bigBytes := totalBytes * 2 / 3
	perBig := bigBytes / int64(bigFiles)
	smallBytes := totalBytes - bigBytes
	perSmall := smallBytes / int64(files)
	if perSmall < 512 {
		perSmall = 512
	}

	write := func(name string, size int64) {
		header := &tar.Header{Name: name, Mode: 0o644, Typeflag: tar.TypeReg, Size: size}
		if err := writer.WriteHeader(header); err != nil {
			tb.Fatal(err)
		}
		for remaining := size; remaining > 0; {
			// Vary the starting offset so files are not byte-identical, and cap
			// the chunk at what is left in both the corpus and the file.
			start := rng.Intn(len(corpus) / 2)
			chunk := int64(len(corpus) - start)
			if chunk > remaining {
				chunk = remaining
			}
			if _, err := writer.Write(corpus[start : start+int(chunk)]); err != nil {
				tb.Fatal(err)
			}
			remaining -= chunk
		}
	}
	for i := 0; i < bigFiles; i++ {
		write(fmt.Sprintf("usr/lib/big/lib%d.so", i), perBig)
	}
	for i := 0; i < files; i++ {
		write(fmt.Sprintf("usr/lib/python3/site-packages/pkg%d/mod%d.py", i%256, i), perSmall)
	}
	if err := writer.Close(); err != nil {
		tb.Fatal(err)
	}

	var gz bytes.Buffer
	zw, _ := kgzip.NewWriterLevel(&gz, kgzip.DefaultCompression)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		tb.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		tb.Fatal(err)
	}
	tb.Logf("synthetic layer: %d files, raw %.2f GiB, gzip %.2f GiB (%.2fx)",
		files+bigFiles, float64(raw.Len())/(1<<30), float64(gz.Len())/(1<<30),
		float64(raw.Len())/float64(gz.Len()))
	return gz.Bytes()
}

// TestBenchUnpack reports the unpack rate for one large layer. It exists to
// compare implementation variants on a fixed workload; run it explicitly:
//
//	go test ./internal/agent/runtime/udocker/native -run TestBenchUnpack -v -timeout 30m
func TestBenchUnpack(t *testing.T) {
	if os.Getenv("GPUWORKS_UNPACK_BENCH") == "" {
		t.Skip("set GPUWORKS_UNPACK_BENCH=1 to run the unpack benchmark")
	}
	files := 20000
	var total int64 = 2 << 30
	data := syntheticLayer(t, files, total)

	// One layer, one worker: this measures the single-stream path that dominates
	// a real image's create time, where the largest layer cannot be split
	// across cores because a DEFLATE stream only decodes front to back.
	root := filepath.Join(t.TempDir(), "ROOT")
	unpacker := &Unpacker{Root: root, Workers: 1}
	start := time.Now()
	err := unpacker.Unpack(context.Background(), []LayerSource{{
		Digest: digestFor(0),
		Size:   int64(len(data)),
		Open:   func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
	}})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("unpacked %.2f GiB raw in %s (%.0f MB/s)",
		float64(total)/(1<<30), elapsed.Round(10*time.Millisecond),
		float64(total)/elapsed.Seconds()/1e6)
}
