package logging

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func write(t *testing.T, w *RotatingWriter, s string) {
	t.Helper()
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatalf("Write(%q) = %v, want nil (logging must never fail the caller)", s, err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	return string(b)
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("%s exists, want it removed", filepath.Base(path))
	}
}

// TestRotatingLogWriterRotatesWhileProcessIsRunning covers the original agent
// behavior: rotation happens mid-run, the active file holds the newest records
// and the previous generation holds the ones before them.
func TestRotatingLogWriterRotatesWhileProcessIsRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	w, err := NewRotatingWriter(path, RotationOptions{MaxBytes: 6 << 20, Keep: 1})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 6*1024*1024)
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if current.Size() != int64(len(payload)) || previous.Size() != int64(len(payload)) {
		t.Fatalf("rotated sizes current=%d previous=%d", current.Size(), previous.Size())
	}
}

func TestRotatingLogWriterKeepsNewestFirstAcrossGenerations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gpuworks-server.log")
	w, err := NewRotatingWriter(path, RotationOptions{MaxBytes: 10, Keep: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, record := range []string{"first", "second", "third", "fourth", "fifth"} {
		write(t, w, record)
	}
	if got := read(t, path); got != "fifth" {
		t.Fatalf("active log = %q, want the newest record", got)
	}
	if got := read(t, path+".1"); got != "fourth" {
		t.Fatalf(".1 = %q, want the second-newest record", got)
	}
	if got := read(t, path+".2"); got != "third" {
		t.Fatalf(".2 = %q", got)
	}
	if got := read(t, path+".3"); got != "second" {
		t.Fatalf(".3 = %q", got)
	}
	mustNotExist(t, path+".4")
	if _, err := os.Stat(path + ".3"); err != nil {
		t.Fatalf(".3 should hold the oldest retained generation: %v", err)
	}
}

func TestRotatingLogWriterKeepZeroDiscardsRotatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	w, err := NewRotatingWriter(path, RotationOptions{MaxBytes: 5, Keep: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	write(t, w, "aaaaa")
	write(t, w, "bbbbb")
	if got := read(t, path); got != "bbbbb" {
		t.Fatalf("active log = %q", got)
	}
	mustNotExist(t, path+".1")
}

// TestRotatingLogWriterRotatesOversizedExistingFile covers a restart after the
// process was killed mid-write: the next start must not append to an
// already-oversized file.
func TestRotatingLogWriterRotatesOversizedExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	if err := os.WriteFile(path, []byte("old-record-that-fills-the-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := NewRotatingWriter(path, RotationOptions{MaxBytes: 8, Keep: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if got := read(t, path); got != "" {
		t.Fatalf("active log = %q, want a fresh file", got)
	}
	if got := read(t, path+".1"); got != "old-record-that-fills-the-file" {
		t.Fatalf(".1 = %q, want the pre-existing content", got)
	}
}

// TestRotatingLogWriterDegradesWhenRotationFails pins the contract that a
// logging failure is reported once and never propagates to the caller.
func TestRotatingLogWriterDegradesWhenRotationFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "server.log")
	w, err := NewRotatingWriter(path, RotationOptions{MaxBytes: 4, Keep: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var reported []error
	w.OnError(func(err error) { reported = append(reported, err) })
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	write(t, w, "aaaa")
	write(t, w, "bbbb") // crosses the cap: rotation fails, writer disables
	write(t, w, "cccc") // discarded silently
	if len(reported) != 1 {
		t.Fatalf("onError calls = %d, want exactly one report", len(reported))
	}
}

func TestRotatingLogWriterDisablesAfterWriteError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	w, err := NewRotatingWriter(path, RotationOptions{MaxBytes: 1 << 20, Keep: 1})
	if err != nil {
		t.Fatal(err)
	}
	var reported []error
	w.OnError(func(err error) { reported = append(reported, err) })
	// Close the descriptor behind the writer to make the next write fail, the
	// way a revoked mount or a full disk would.
	if err := w.f.Close(); err != nil {
		t.Fatal(err)
	}
	write(t, w, "record")
	if len(reported) != 1 {
		t.Fatalf("onError calls = %d, want one", len(reported))
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close after failure = %v, want nil", err)
	}
}

func TestNewLoggerWritesToFileAndStderr(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logDir, "gpuworks-server.log")
	base := slog.New(slog.NewTextHandler(io.Discard, nil))
	log, closeLog := NewLogger(base, FileOptions{Path: path, MaxBytes: 1 << 20, Keep: 3})
	log.Info("server listening", "addr", ":8800")
	closeLog()
	got := read(t, path)
	if !strings.Contains(got, "server listening") || !strings.Contains(got, ":8800") {
		t.Fatalf("log file = %q, want the emitted record", got)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("log file mode = %v, want 0600", info.Mode().Perm())
	}
}

// TestNewLoggerFallsBackToStderrWhenFileIsUnavailable keeps startup working
// when the data directory is read-only or the disk is full.
func TestNewLoggerFallsBackToStderrWhenFileIsUnavailable(t *testing.T) {
	var stderr bytes.Buffer
	base := slog.New(slog.NewTextHandler(&stderr, nil))
	missing := filepath.Join(t.TempDir(), "absent", "gpuworks-server.log")
	for _, fileOnly := range []bool{false, true} {
		stderr.Reset()
		log, closeLog := NewLogger(base, FileOptions{Path: missing, FileOnly: fileOnly})
		log.Info("still usable")
		closeLog()
		if got := stderr.String(); !strings.Contains(got, "log file unavailable") || !strings.Contains(got, "still usable") {
			t.Fatalf("file_only=%v: stderr fallback missing warning or record: %s", fileOnly, got)
		}
		if _, err := os.Stat(missing); err == nil {
			t.Fatal("NewLogger created a file in a missing directory")
		}
	}
	plain, closePlain := NewLogger(base, FileOptions{})
	if plain != base {
		t.Fatal("empty path should return the base logger unchanged")
	}
	closePlain()
}

func TestParseSize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"10MB", 10 << 20},
		{"10mb", 10 << 20},
		{"512KB", 512 << 10},
		{"1GB", 1 << 30},
		{"1024B", 1024},
		{"2048", 2048},
		{" 5 MB ", 5 << 20},
	} {
		got, err := ParseSize(tc.in)
		if err != nil {
			t.Fatalf("ParseSize(%q) error = %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ParseSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "abc", "0", "-1MB", "10XB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Fatalf("ParseSize(%q) = nil error, want rejection", bad)
		}
	}
}

// FileOnly keeps records out of stderr: under launchd stderr is a file
// nothing rotates, and a copy of every record there only fills the disk.
func TestNewLoggerFileOnlyLeavesStderrAlone(t *testing.T) {
	stderrPath := filepath.Join(t.TempDir(), "stderr")
	stderr, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = stderr
	t.Cleanup(func() { os.Stderr = saved; stderr.Close() })
	path := filepath.Join(t.TempDir(), "agent.log")
	for _, fileOnly := range []bool{true, false} {
		log, closeLog := NewLogger(slog.New(slog.NewTextHandler(io.Discard, nil)), FileOptions{Path: path, FileOnly: fileOnly})
		log.Info("registered", "file_only", fileOnly)
		closeLog()
	}
	got := read(t, stderrPath)
	if strings.Contains(got, "file_only=true") || !strings.Contains(got, "file_only=false") {
		t.Fatalf("stderr = %q", got)
	}
	if file := read(t, path); !strings.Contains(file, "file_only=true") || !strings.Contains(file, "file_only=false") {
		t.Fatalf("log file = %q", file)
	}
}

func TestStderrIsAFileOnlyForARegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stderr.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 1025), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	wantFileOnly := runtime.GOOS == "darwin"
	if got := StderrIsAFile(f, 1024); got != wantFileOnly {
		t.Errorf("StderrIsAFile = %v, want %v on %s", got, wantFileOnly, runtime.GOOS)
	}
	wantSize := int64(1025)
	if wantFileOnly {
		wantSize = 0
	}
	if info, _ := os.Stat(path); info.Size() != wantSize {
		t.Fatalf("stderr size = %d, want %d on %s", info.Size(), wantSize, runtime.GOOS)
	}
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if StderrIsAFile(null, 1024) {
		t.Fatal("/dev/null taken for a file")
	}
}

func TestStderrPipeAndClosedFileAreNotFileOnly(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if StderrIsAFile(w, 1) {
		t.Fatal("a pipe must retain stderr logging")
	}
	f, err := os.CreateTemp(t.TempDir(), "closed-stderr")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if StderrIsAFile(f, 1) {
		t.Fatal("a closed stderr must not select file-only logging")
	}
}
