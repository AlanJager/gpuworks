package imagearchive

import (
	"bytes"
	"context"
	"errors"
	"io"
)

// rangeReader lets archive/tar seek past layer bodies. Reuse the tail from the
// tag lookup and coalesce adjacent header/config reads into small requests.
type rangeReader struct {
	ctx      context.Context
	f        *fetcher
	size     int64
	tail     []byte
	start    int64
	window   []byte
	requests int
}

func (r *rangeReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.size {
		return 0, io.EOF
	}
	tailStart := r.size - int64(len(r.tail))
	if off >= tailStart {
		return bytes.NewReader(r.tail).ReadAt(p, off-tailStart)
	}
	if off >= r.start && off+int64(len(p)) <= r.start+int64(len(r.window)) {
		return copy(p, r.window[off-r.start:]), nil
	}
	// Keep both bandwidth and round trips bounded for fragmented archives.
	const maxRequests = 64
	if r.requests >= maxRequests || r.f.remaining < int64(len(p)) {
		return 0, errors.New("archive metadata request budget exhausted")
	}
	want := min(max(int64(len(p)), 16<<10), r.f.remaining, r.size-off)
	r.requests++
	window, err := r.f.rangeGet(r.ctx, off, off+want-1)
	if err != nil {
		return 0, err
	}
	r.start, r.window = off, window
	n := copy(p, window)
	if n < len(p) {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

func (f *fetcher) configMetadata(ctx context.Context, size int64, tail, manifest []byte) map[string]ImageMetadata {
	source := &rangeReader{ctx: ctx, f: f, size: size, tail: tail}
	var metadata archiveMetadata
	metadata.setDockerManifest(manifest)
	result, _ := metadata.readTar(ctx, io.NewSectionReader(source, 0, size), nil)
	// Enrichment is optional. A budget limit or an unreadable config must
	// preserve the tags already obtained by the URL preview.
	return result.MetadataByTag
}
