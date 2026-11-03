package debuglog

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSessionGetsDebugWhileTheProcessLogKeepsItsLevel(t *testing.T) {
	var main strings.Builder
	tee := New(slog.NewTextHandler(&main, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log := slog.New(tee).With("component", "hub")

	log.Debug("before")
	if log.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug enabled with no session")
	}
	path := filepath.Join(t.TempDir(), "debug", "s1.log")
	if err := tee.Start("s1", path, time.Now().Add(time.Hour), 1<<20); err != nil {
		t.Fatal(err)
	}
	log.Debug("runtime command", "args", "image inspect x")
	log.Info("agent starting")
	tee.Stop("")

	if strings.Contains(main.String(), "runtime command") || !strings.Contains(main.String(), "agent starting") {
		t.Fatalf("process log:\n%s", main.String())
	}
	session := read(t, path)
	if !strings.Contains(session, `level=DEBUG msg="runtime command" args="image inspect x" component=hub`) ||
		!strings.Contains(session, `msg="agent starting" component=hub`) || strings.Contains(session, "before") ||
		!strings.Contains(session, `msg="debug session ended" reason=stopped`) {
		t.Fatalf("session file:\n%s", session)
	}
	st, ok := tee.Status()
	if !ok || st.Open || st.Ended != "stopped" || st.ID != "s1" || st.Written == 0 {
		t.Fatalf("status = %+v", st)
	}
	log.Debug("after")
	if strings.Contains(read(t, path), "after") {
		t.Fatal("written after stop")
	}
}

func TestSessionEndsAtItsDeadline(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tee := New(slog.NewTextHandler(io.Discard, nil))
	tee.SetClock(func() time.Time { return now })
	path := filepath.Join(t.TempDir(), "s.log")
	if err := tee.Start("s", path, now.Add(10*time.Minute), 1<<20); err != nil {
		t.Fatal(err)
	}
	log := slog.New(tee)
	log.Debug("inside")
	now = now.Add(10 * time.Minute)
	log.Debug("outside")
	if log.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("still enabled past the deadline")
	}
	got := read(t, path)
	if !strings.Contains(got, "inside") || strings.Contains(got, "outside") || !strings.Contains(got, "reason=deadline") {
		t.Fatalf("session:\n%s", got)
	}
}

func TestSessionEndsAtItsSizeLimit(t *testing.T) {
	tee := New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "s.log")
	if err := tee.Start("s", path, time.Now().Add(time.Hour), 400); err != nil {
		t.Fatal(err)
	}
	log := slog.New(tee)
	for i := 0; i < 100; i++ {
		log.Debug("a record of some length", "i", i)
	}
	got := read(t, path)
	st, _ := tee.Status()
	if st.Open || st.Ended != "size_limit" || st.Written > 400 || !strings.Contains(got, "reason=size_limit") {
		t.Fatalf("status %+v\n%s", st, got)
	}
}

func TestStartingASessionEndsTheOpenOne(t *testing.T) {
	tee := New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	_ = tee.Start("a", filepath.Join(dir, "a.log"), time.Now().Add(time.Hour), 1<<20)
	_ = tee.Start("b", filepath.Join(dir, "b.log"), time.Now().Add(time.Hour), 1<<20)
	slog.New(tee).Debug("to b")
	if !strings.Contains(read(t, filepath.Join(dir, "a.log")), "reason=stopped") || !strings.Contains(read(t, filepath.Join(dir, "b.log")), "to b") {
		t.Fatal("the second session did not replace the first")
	}
	if tee.Stop("a") {
		t.Fatal("stopped a session that is not open")
	}
	if !tee.Stop("b") {
		t.Fatal("did not stop the open session")
	}
}

func TestConcurrentWritesAndStop(t *testing.T) {
	tee := New(slog.NewTextHandler(io.Discard, nil))
	_ = tee.Start("s", filepath.Join(t.TempDir(), "s.log"), time.Now().Add(time.Hour), 64<<10)
	log := slog.New(tee)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				log.Debug("x", "i", i)
			}
		}()
	}
	tee.Stop("s")
	wg.Wait()
}

func TestSweepRemovesOldFiles(t *testing.T) {
	dir := t.TempDir()
	old, fresh := filepath.Join(dir, "old.log"), filepath.Join(dir, "fresh.log")
	_ = os.WriteFile(old, []byte("x"), 0o600)
	_ = os.WriteFile(fresh, []byte("x"), 0o600)
	past := time.Now().Add(-8 * 24 * time.Hour)
	_ = os.Chtimes(old, past, past)
	Sweep(dir, 7*24*time.Hour, time.Now())
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old file kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh file removed")
	}
}

func BenchmarkNoSession(b *testing.B) {
	log := slog.New(New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})))
	for i := 0; i < b.N; i++ {
		log.Debug("skipped", "i", i)
	}
}
