package api

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	benchmeta "gpuworks.ai/gpuworks/pkg/bench"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

type benchmarkDetail struct {
	mu     sync.Mutex
	file   *os.File
	gz     *gzip.Writer
	cw     *limitedWriter
	path   string
	closed bool
	sealed bool
	runID  uint
	seq    int
}

type limitedWriter struct {
	w         io.Writer
	n, max    int64
	truncated bool
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.truncated || w.n+int64(len(p)) > w.max {
		w.truncated = true
		// Records are all-or-nothing: a partial JSON line would make the retained
		// artifact unusable. Report the input consumed so callers can seal it with
		// an explicit marker and still close the gzip stream cleanly.
		return len(p), nil
	}
	n, err := w.w.Write(p)
	w.n += int64(n)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

func newBenchmarkDetail(root string, runID uint, seq int, keep bool, capMB int) (*benchmarkDetail, error) {
	if !keep || root == "" {
		return nil, nil
	}
	if capMB <= 0 {
		capMB = benchmeta.DefaultDetailCapMB
	}
	if capMB > benchmeta.MaxDetailCapMB {
		capMB = benchmeta.MaxDetailCapMB
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, err
	}
	path := filepath.Join(root, fmt.Sprintf("run-%d-point-%d.jsonl.gz", runID, seq))
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	gz := gzip.NewWriter(f)
	cw := &limitedWriter{w: gz, max: int64(capMB)*1024*1024 - 4096}
	return &benchmarkDetail{file: f, cw: cw, gz: gz, path: path, runID: runID, seq: seq}, nil
}

func (d *benchmarkDetail) Add(r loadgen.Result) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.sealed {
		return
	}
	record := loadgen.NewDetailRecord(r)
	record.RunID, record.PointSeq = d.runID, d.seq
	b, _ := json.Marshal(record)
	b = append(b, '\n')
	if _, err := d.cw.Write(b); err != nil {
		d.sealed = true
		return
	}
	if d.cw.truncated {
		_, _ = d.gz.Write([]byte("{\"truncated\":true}\n"))
		d.sealed = true
	}
}

func (d *benchmarkDetail) Close() (string, error) {
	if d == nil {
		return "", nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return d.path, nil
	}
	d.closed = true
	err := d.gz.Close()
	if e := d.file.Close(); err == nil {
		err = e
	}
	if err != nil {
		_ = os.Remove(d.path)
		return "", err
	}
	return d.path, err
}
