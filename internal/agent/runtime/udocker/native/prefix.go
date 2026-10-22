package native

import (
	"errors"
	"io"
	"os"
	"sync"
)

// growingFile tracks the contiguous prefix of a file that several range
// requests are filling out of order, so a sequential consumer can start
// reading before the whole file has arrived.
//
// This is what lets a layer's gzip stream be inflated while it is still
// downloading. A DEFLATE stream can only be decoded front to back, and the
// downloader deliberately splits a large blob across several connections
// (one stream rarely saturates the link), so the bytes do not land in order.
// Each part writes sequentially within its own span, which means the
// contiguous prefix is simply the leading run of filled spans plus however far
// the next one has got.
type growingFile struct {
	mu       sync.Mutex
	ready    *sync.Cond
	chunk    int64   // nominal span length handed to each part
	size     int64   // total blob size
	progress []int64 // bytes written per part
	failed   error
	finished bool
}

func newGrowingFile(size, chunk int64, parts int) *growingFile {
	g := &growingFile{chunk: chunk, size: size, progress: make([]int64, parts)}
	g.ready = sync.NewCond(&g.mu)
	return g
}

// advance records that part index has written n more bytes.
func (g *growingFile) advance(index int, n int64) {
	g.mu.Lock()
	g.progress[index] += n
	g.mu.Unlock()
	g.ready.Broadcast()
}

// finish marks the download complete, or failed when err is non-nil. A reader
// blocked on more bytes is always released by this.
func (g *growingFile) finish(err error) {
	g.mu.Lock()
	if err != nil && g.failed == nil {
		g.failed = err
	}
	g.finished = true
	g.mu.Unlock()
	g.ready.Broadcast()
}

// prefixLocked is the number of bytes readable from offset zero.
func (g *growingFile) prefixLocked() int64 {
	var prefix int64
	for index, written := range g.progress {
		span := g.chunk
		if start := int64(index) * g.chunk; start+span > g.size {
			span = g.size - start
		}
		prefix += written
		if written < span {
			break // this part is still filling; nothing beyond it is contiguous
		}
	}
	if prefix > g.size {
		prefix = g.size
	}
	return prefix
}

// waitFor blocks until at least want bytes are contiguous, the download
// finishes, or it fails. It returns the readable prefix.
func (g *growingFile) waitFor(want int64) (int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for {
		prefix := g.prefixLocked()
		if g.failed != nil {
			return prefix, g.failed
		}
		if prefix >= want || g.finished {
			return prefix, nil
		}
		g.ready.Wait()
	}
}

// prefixReader reads a file sequentially, never passing the contiguous prefix
// its growingFile reports. Reads block while the download catches up, so the
// consumer runs exactly as fast as the slower of inflate and network.
type prefixReader struct {
	file   *os.File
	growth *growingFile
	offset int64
}

func newPrefixReader(file *os.File, growth *growingFile) *prefixReader {
	return &prefixReader{file: file, growth: growth}
}

func (r *prefixReader) Read(p []byte) (int, error) {
	prefix, err := r.growth.waitFor(r.offset + 1)
	if err != nil {
		return 0, err
	}
	if prefix <= r.offset {
		// The download finished without producing more bytes. Anything short of
		// the declared size is a truncated transfer; the caller retries.
		if r.offset >= r.growth.size {
			return 0, io.EOF
		}
		return 0, io.ErrUnexpectedEOF
	}
	if available := prefix - r.offset; int64(len(p)) > available {
		p = p[:available]
	}
	n, err := r.file.ReadAt(p, r.offset)
	r.offset += int64(n)
	if errors.Is(err, io.EOF) && n > 0 {
		err = nil
	}
	return n, err
}
