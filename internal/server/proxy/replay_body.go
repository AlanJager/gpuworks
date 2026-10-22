package proxy

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

var replayBudget atomic.Int64

type replayBody struct {
	mem        []byte
	file       *os.File
	size       int64
	budgetHeld int64
	once       sync.Once
}

func newReplayBody(data []byte, maxBody int64) (*replayBody, error) {
	if maxBody > 0 && int64(len(data)) > maxBody {
		return nil, tooLarge(maxBody)
	}
	b := &replayBody{size: int64(len(data))}
	if len(data) <= bodyMemoryThreshold && reserveReplayBytes(int64(len(data))) {
		b.mem = append([]byte(nil), data...)
		b.budgetHeld = int64(len(data))
		return b, nil
	}
	f, err := os.CreateTemp("", "gpuworks-proxy-body-*")
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	b.file = f
	return b, nil
}

func newReplayBodyFromReader(r io.Reader, maxBody int64) (*replayBody, error) {
	writer := newReplayWriter(maxBody)
	_, err := io.Copy(writer, r)
	if err != nil {
		writer.Abort()
		return nil, err
	}
	return writer.Close()
}

// replayWriter keeps small bodies in memory and switches to a temporary file
// as soon as the memory threshold is crossed. It deliberately exposes only a
// Writer so callers never need a second full-body buffer while transforming a
// request.
type replayWriter struct {
	mem     bytes.Buffer
	file    *os.File
	size    int64
	maxBody int64
}

func newReplayWriter(maxBody int64) *replayWriter {
	return &replayWriter{maxBody: maxBody}
}

func (w *replayWriter) Write(p []byte) (int, error) {
	if w.maxBody > 0 && w.size+int64(len(p)) > w.maxBody {
		return 0, tooLarge(w.maxBody)
	}
	if w.file == nil && w.size+int64(len(p)) <= bodyMemoryThreshold {
		n, err := w.mem.Write(p)
		w.size += int64(n)
		return n, err
	}
	if w.file == nil {
		f, err := os.CreateTemp("", "gpuworks-proxy-body-*")
		if err != nil {
			return 0, err
		}
		if _, err := f.Write(w.mem.Bytes()); err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
			return 0, err
		}
		w.file = f
		w.mem.Reset()
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *replayWriter) Close() (*replayBody, error) {
	if w.file != nil {
		if _, err := w.file.Seek(0, io.SeekStart); err != nil {
			name := w.file.Name()
			_ = w.file.Close()
			_ = os.Remove(name)
			return nil, err
		}
		return &replayBody{file: w.file, size: w.size}, nil
	}
	data := w.mem.Bytes()
	if reserveReplayBytes(int64(len(data))) {
		return &replayBody{mem: data, size: int64(len(data)), budgetHeld: int64(len(data))}, nil
	}
	f, err := os.CreateTemp("", "gpuworks-proxy-body-*")
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	return &replayBody{file: f, size: int64(len(data))}, nil
}

func (w *replayWriter) Abort() {
	if w == nil || w.file == nil {
		return
	}
	name := w.file.Name()
	_ = w.file.Close()
	_ = os.Remove(name)
	w.file = nil
}

func reserveReplayBytes(n int64) bool {
	if n <= 0 {
		return true
	}
	for {
		old := replayBudget.Load()
		if old+n > bodyMemoryBudget {
			return false
		}
		if replayBudget.CompareAndSwap(old, old+n) {
			return true
		}
	}
}

func (b *replayBody) NewReader() io.ReadCloser {
	if b == nil {
		return io.NopCloser(bytes.NewReader(nil))
	}
	if b.mem != nil {
		return io.NopCloser(bytes.NewReader(b.mem))
	}
	if b.file == nil {
		return io.NopCloser(bytes.NewReader(nil))
	}
	return io.NopCloser(io.NewSectionReader(b.file, 0, b.size))
}

func (b *replayBody) newReadSeeker() io.ReadSeeker {
	if b == nil {
		return bytes.NewReader(nil)
	}
	if b.mem != nil {
		return bytes.NewReader(b.mem)
	}
	if b.file == nil {
		return bytes.NewReader(nil)
	}
	return io.NewSectionReader(b.file, 0, b.size)
}

func (b *replayBody) Close() error {
	if b == nil {
		return nil
	}
	var err error
	b.once.Do(func() {
		if b.budgetHeld > 0 {
			replayBudget.Add(-b.budgetHeld)
			b.budgetHeld = 0
		}
		if b.file != nil {
			name := b.file.Name()
			if e := b.file.Close(); e != nil {
				err = e
			}
			if e := os.Remove(name); e != nil && !errors.Is(e, os.ErrNotExist) && err == nil {
				err = e
			}
		}
	})
	return err
}
