package logging

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeClock drives a writer's resume wait.
type fakeClock struct{ at time.Time }

func (c *fakeClock) now() time.Time          { return c.at }
func (c *fakeClock) advance(d time.Duration) { c.at = c.at.Add(d) }

func newClockedWriter(t *testing.T, path string, opts RotationOptions) (*RotatingWriter, *fakeClock) {
	t.Helper()
	w, err := NewRotatingWriter(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{at: time.Date(2026, 11, 6, 7, 17, 57, 0, time.UTC)}
	w.now = clock.now
	t.Cleanup(func() { _ = w.Close() })
	return w, clock
}

var resumedLine = regexp.MustCompile(`^time=(\S+) level=WARN msg="log file writes resumed" dropped=(\d+) since=(\S+) err=(".*")$`)

// TestRotatingLogWriterResumesAfterTheDiskFillsUp writes to /dev/full, which
// fails every write with the ENOSPC a full disk returns, then lets the disk
// "free up" by putting the real file back.
func TestRotatingLogWriterResumesAfterTheDiskFillsUp(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/dev/full is Linux's")
	}
	path := filepath.Join(t.TempDir(), "server.log")
	w, clock := newClockedWriter(t, path, RotationOptions{MaxBytes: 1 << 20, Keep: 1})
	var reported []error
	w.OnError(func(err error) { reported = append(reported, err) })
	write(t, w, "before\n")

	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("no /dev/full: %v", err)
	}
	_ = w.f.Close()
	w.f = full
	write(t, w, "lost-1\n") // ENOSPC: the writer disables
	clock.advance(10 * time.Second)
	write(t, w, "lost-2\n") // inside the wait: dropped without a try
	if len(reported) != 1 || !strings.Contains(reported[0].Error(), "no space left on device") {
		t.Fatalf("reported = %v, want one ENOSPC report", reported)
	}
	if got := read(t, path); got != "before\n" {
		t.Fatalf("log while disabled = %q", got)
	}

	clock.advance(resumeInterval)
	write(t, w, "after\n")
	lines := strings.Split(strings.TrimSuffix(read(t, path), "\n"), "\n")
	if len(lines) != 3 || lines[0] != "before" || lines[2] != "after" {
		t.Fatalf("log after resuming = %q", lines)
	}
	m := resumedLine.FindStringSubmatch(lines[1])
	if m == nil {
		t.Fatalf("resume line = %q", lines[1])
	}
	if m[1] != clock.at.Format(time.RFC3339Nano) || m[2] != "2" || m[3] != "2026-11-06T07:17:57Z" || !strings.Contains(m[4], "no space left on device") {
		t.Fatalf("resume line = %q, want now, dropped=2, the outage start and the ENOSPC cause", lines[1])
	}
	if len(reported) != 1 {
		t.Fatalf("reported = %d, want the outage reported once", len(reported))
	}
}

// TestRotatingLogWriterKeepsTryingWhileTheFileIsUnavailable: a try that fails
// waits another interval, reports nothing new, and the next one that works
// counts every record dropped across the tries.
func TestRotatingLogWriterKeepsTryingWhileTheFileIsUnavailable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "server.log")
	w, clock := newClockedWriter(t, path, RotationOptions{MaxBytes: 4, Keep: 2})
	var reported []error
	w.OnError(func(err error) { reported = append(reported, err) })
	write(t, w, "aaaa")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	write(t, w, "bbbb") // rotation fails
	clock.advance(resumeInterval)
	write(t, w, "cccc") // try fails: the directory is still gone
	write(t, w, "dddd") // inside the next wait
	if len(reported) != 1 {
		t.Fatalf("reported = %d, want one", len(reported))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	clock.advance(resumeInterval)
	write(t, w, "eeee")
	// The resume line alone crosses the 4-byte cap, so the record after it
	// starts the next generation.
	m := resumedLine.FindStringSubmatch(strings.TrimSuffix(read(t, path+".1"), "\n"))
	if m == nil || m[2] != "3" {
		t.Fatalf("server.log.1 = %q, want the resume line with dropped=3", read(t, path+".1"))
	}
	if got := read(t, path); got != "eeee" {
		t.Fatalf("server.log = %q, want the record after the resume line", got)
	}
}

// TestRotatingLogWriterEndsTheLineAFailedWriteCut: a write the disk cut short
// leaves half a record; the resume line starts on a line of its own so the
// next reader still finds every timestamp at the start of a line.
func TestRotatingLogWriterEndsTheLineAFailedWriteCut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	w, clock := newClockedWriter(t, path, RotationOptions{MaxBytes: 1 << 20, Keep: 1})
	write(t, w, "time=2026-11-06T07:17:57Z msg=\"http request\" path=/api/v1/images s")
	w.mu.Lock()
	w.disable(errors.New("write server.log: no space left on device"), true)
	w.mu.Unlock()
	clock.advance(resumeInterval)
	write(t, w, "next\n")
	lines := strings.Split(read(t, path), "\n")
	if len(lines) != 4 || !strings.HasSuffix(lines[0], "path=/api/v1/images s") || resumedLine.FindString(lines[1]) == "" || lines[2] != "next" {
		t.Fatalf("log = %q", lines)
	}
}

func TestRotatingLogWriterDoesNotResumeAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	w, clock := newClockedWriter(t, path, RotationOptions{MaxBytes: 1 << 20, Keep: 1})
	w.mu.Lock()
	w.disable(errors.New("boom"), false)
	w.mu.Unlock()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	clock.advance(resumeInterval)
	write(t, w, "late\n")
	if got := read(t, path); got != "" {
		t.Fatalf("log after Close = %q, want nothing", got)
	}
}
