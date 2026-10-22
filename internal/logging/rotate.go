// Package logging carries the log plumbing shared by the gpuworks server and
// agent binaries: a size-based rotating file writer with a bounded number of
// retained generations, and the logger assembly built on top of it.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// RotationOptions configures a RotatingWriter.
//
// MaxBytes is the size at which the current file is rotated; a write that would
// cross the cap rotates first, so a file may exceed MaxBytes by at most the
// length of one record once the first record of a fresh file is written.
// Keep is how many rotated generations are retained: <path>.1 is the newest,
// <path>.Keep the oldest, and older files are deleted during rotation.
type RotationOptions struct {
	MaxBytes int64
	Keep     int
}

// DefaultRotation is the server default: 10 MiB per file, 20 generations.
func DefaultRotation() RotationOptions {
	return RotationOptions{MaxBytes: 10 << 20, Keep: 20}
}

// RotatingWriter is an io.WriteCloser that rotates its file by size.
//
// It is log plumbing, not a data path: once rotation or a write fails the
// writer disables itself, reports the failure through OnError and silently
// discards further writes, so a full disk cannot take the process down or
// drown it in repeated errors. A nil *RotatingWriter is not usable; callers
// that may not have a file use NewLogger instead.
type RotatingWriter struct {
	mu       sync.Mutex
	path     string
	opts     RotationOptions
	f        *os.File
	size     int64
	disabled bool
	onError  func(error)
}

// NewRotatingWriter opens path for appending, rotating first when the existing
// file already exceeds the cap. The parent directory must exist; callers own
// its permissions: the agent starts with 0700 and may later grant a dedicated
// runtime user search-only traversal (docs/reference/security.md), the server
// uses 0755. Log files keep their own private permissions.
func NewRotatingWriter(path string, opts RotationOptions) (*RotatingWriter, error) {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultRotation().MaxBytes
	}
	if opts.Keep < 0 {
		opts.Keep = 0
	}
	w := &RotatingWriter{path: filepath.Clean(path), opts: opts}
	if info, err := os.Stat(w.path); err == nil {
		w.size = info.Size()
		if w.size >= opts.MaxBytes {
			if err := w.rotate(); err != nil {
				return nil, err
			}
		}
	}
	if w.f == nil {
		f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
		w.f = f
	}
	return w, nil
}

// OnError registers a callback invoked at most once, when the writer gives up.
func (w *RotatingWriter) OnError(f func(error)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.onError = f
}

// Write appends p, rotating first if it would cross the size cap. Write never
// returns an error: logging must not fail the operation being logged.
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.disabled {
		return len(p), nil
	}
	if w.size > 0 && w.size+int64(len(p)) > w.opts.MaxBytes {
		if err := w.rotate(); err != nil {
			w.disable(err)
			return len(p), nil
		}
	}
	if w.f == nil {
		return len(p), nil
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	if err != nil {
		w.disable(err)
		return len(p), nil
	}
	return n, nil
}

// Close closes the current file. The writer is unusable afterwards.
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// rotate closes the current file and shifts the generations: path.Keep is
// dropped, path.Keep-1 → path.Keep … path.1 → path.2, path → path.1, and a
// fresh path is opened. Callers must hold w.mu.
func (w *RotatingWriter) rotate() error {
	if w.f != nil {
		if err := w.f.Close(); err != nil {
			return err
		}
		w.f = nil
	}
	if w.opts.Keep <= 0 {
		if err := os.Remove(w.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else {
		if err := os.Remove(rotatedName(w.path, w.opts.Keep)); err != nil && !os.IsNotExist(err) {
			return err
		}
		for i := w.opts.Keep - 1; i >= 1; i-- {
			if err := os.Rename(rotatedName(w.path, i), rotatedName(w.path, i+1)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if err := os.Rename(w.path, rotatedName(w.path, 1)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w.f, w.size = f, 0
	return nil
}

// disable reports the failure once and drops every later write. Callers must
// hold w.mu.
func (w *RotatingWriter) disable(err error) {
	w.disabled = true
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	if w.onError != nil {
		w.onError(err)
	}
}

func rotatedName(path string, generation int) string {
	return path + "." + strconv.Itoa(generation)
}

// FileOptions configures NewLogger.
type FileOptions struct {
	Path     string // empty = stderr only
	MaxBytes int64  // rotation size, DefaultRotation when zero
	Keep     int    // retained generations
	Level    slog.Level
	// FileOnly writes records to the file alone, not also to stderr: for a
	// stderr that is itself a file nothing rotates (a launchd job's
	// StandardErrorPath), where a copy of every record only fills the disk.
	FileOnly bool
}

// StderrIsAFile reports a macOS stderr that is a regular file, which is what a
// launchd job's StandardErrorPath is (systemd's is a socket to the journal, a
// console's a terminal). Nothing rotates it, so a process logging to its own
// file passes FileOnly and leaves this one to what precedes that log and to
// what the Go runtime itself writes there (a panic); once it has outgrown
// maxBytes, the log's own bound, it is emptied.
func StderrIsAFile(stderr *os.File, maxBytes int64) bool {
	// Keep this launchd heuristic on macOS. On other platforms a regular
	// file is ordinary shell redirection: preserve both its contents and
	// subsequent log records, just as for a terminal or journald socket.
	if runtime.GOOS != "darwin" {
		return false
	}
	info, err := stderr.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if maxBytes > 0 && info.Size() > maxBytes {
		_ = stderr.Truncate(0)
	}
	return true
}

// NewLogger returns base's handler duplicated to an optional rotating file:
// every record is written to stderr (so systemd/journald and console runs keep
// working) and, when Path is set, to that file (FileOnly drops the stderr copy). A file that cannot be opened
// degrades to stderr with a warning rather than failing startup — losing logs
// must not stop the process. The returned closer flushes the file.
func NewLogger(base *slog.Logger, opts FileOptions) (*slog.Logger, func()) {
	if strings.TrimSpace(opts.Path) == "" {
		return base, func() {}
	}
	// Detect shell redirection to our own log before opening the rotating
	// writer: startup rotation can replace the path's inode. A second writer
	// would duplicate records and keep appending to an unlinked generation.
	if !opts.FileOnly {
		if target, err := os.Stat(opts.Path); err == nil {
			if stderr, err := os.Stderr.Stat(); err == nil && os.SameFile(target, stderr) {
				opts.FileOnly = true
			}
		}
	}
	w, err := NewRotatingWriter(opts.Path, RotationOptions{MaxBytes: opts.MaxBytes, Keep: opts.Keep})
	if err != nil {
		base.Warn("log file unavailable; using stderr", "path", opts.Path, "err", err)
		return base, func() {}
	}
	w.OnError(func(err error) {
		base.Warn("log rotation failed; using stderr", "path", opts.Path, "err", err)
	})
	// RotatingWriter consumes the full record even when disabled or failing.
	// Write it first so a stderr failure cannot short-circuit the healthy file;
	// its non-failing Write contract also ensures stderr is always attempted.
	var out io.Writer = io.MultiWriter(w, os.Stderr)
	if opts.FileOnly {
		out = w
	}
	h := slog.NewTextHandler(out, &slog.HandlerOptions{Level: opts.Level})
	return slog.New(h), func() { _ = w.Close() }
}

// ParseSize parses a byte size with an optional KB/MB/GB suffix. Suffixes are
// 1024-based: "10MB" is 10 MiB, matching what the CLI help promises.
func ParseSize(value string) (int64, error) {
	s := strings.TrimSpace(value)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	upper := strings.ToUpper(s)
	multiplier := int64(1)
	for _, suffix := range []struct {
		name string
		mult int64
	}{
		{"GB", 1 << 30},
		{"MB", 1 << 20},
		{"KB", 1 << 10},
		{"B", 1},
	} {
		if strings.HasSuffix(upper, suffix.name) {
			multiplier = suffix.mult
			s = strings.TrimSpace(s[:len(s)-len(suffix.name)])
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", value)
	}
	if n <= 0 {
		return 0, fmt.Errorf("size must be positive: %q", value)
	}
	return n * multiplier, nil
}
