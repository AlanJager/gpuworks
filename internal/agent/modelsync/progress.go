package modelsync

import (
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/traffic"
)

type measuredReader struct {
	r          io.Reader
	hash       hash.Hash
	bytes      int64
	maxBytes   int64
	progress   func(int64)
	last       int64
	lastReport time.Time
}

func newMeasuredReader(r io.Reader, maxBytes int64, progress func(int64)) *measuredReader {
	return &measuredReader{r: r, hash: sha256.New(), maxBytes: maxBytes, progress: progress, lastReport: time.Now()}
}

func (r *measuredReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		traffic.AddStorageSyncRx(n)
		r.bytes += int64(n)
		_, _ = r.hash.Write(p[:n])
		if r.maxBytes > 0 && r.bytes > r.maxBytes {
			return n, fmt.Errorf("model sync archive exceeds %d-byte limit", r.maxBytes)
		}
		if r.progress != nil && (r.bytes-r.last >= 8<<20 || time.Since(r.lastReport) >= time.Second || err == io.EOF) {
			r.last = r.bytes
			r.lastReport = time.Now()
			r.progress(r.bytes)
		}
	}
	return n, err
}

func (r *measuredReader) finishProgress() {
	if r.progress != nil && r.bytes != r.last {
		r.last = r.bytes
		r.progress(r.bytes)
	}
}
