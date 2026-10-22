package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoggerSameStderrDestinationStaysWithinRotation(t *testing.T) {
	for _, alias := range []string{"direct", "hardlink", "symlink"} {
		for _, oversized := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/oversized=%t", alias, oversized), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "server.log")
				initial := ""
				if oversized {
					initial = strings.Repeat("x", 256)
				}
				if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
					t.Fatal(err)
				}
				stderrPath := path
				if alias != "direct" {
					stderrPath = filepath.Join(dir, "stderr")
					link := os.Link
					if alias == "symlink" {
						link = os.Symlink
					}
					if err := link(path, stderrPath); err != nil {
						t.Fatal(err)
					}
				}
				stderr, err := os.OpenFile(stderrPath, os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer stderr.Close()
				saved := os.Stderr
				os.Stderr = stderr
				defer func() { os.Stderr = saved }()
				log, closeLog := NewLogger(slog.New(slog.NewTextHandler(io.Discard, nil)), FileOptions{
					Path: path, MaxBytes: 128, Keep: 1,
				})
				defer closeLog()
				log.Info("unique-marker")
				if got := strings.Count(read(t, path), "unique-marker"); got != 1 {
					t.Fatalf("active file has %d copies of one record", got)
				}
				for i := 0; i < 20; i++ {
					log.Info("rotate", "record", i)
				}
				before, err := stderr.Stat()
				if err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 20; i++ {
					log.Info("after-rotation", "record", i)
				}
				after, err := stderr.Stat()
				if err != nil {
					t.Fatal(err)
				}
				if after.Size() != before.Size() {
					t.Fatalf("retired stderr inode still growing: %d -> %d", before.Size(), after.Size())
				}
				if oversized && after.Size() != int64(len(initial)) {
					t.Fatalf("startup-rotated inode changed: size=%d", after.Size())
				}
				if got := read(t, path); !strings.Contains(got, "after-rotation") || !strings.Contains(got, "record=19") {
					t.Fatalf("latest record missing: %q", got)
				}
				for _, name := range []string{path, path + ".1"} {
					info, err := os.Stat(name)
					if err != nil {
						t.Fatal(err)
					}
					if info.Size() > 128 {
						t.Fatalf("rotation cap exceeded: %s size=%d", name, info.Size())
					}
				}
			})
		}
	}
}

func TestLoggerUnwritableStderrPreservesHealthyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stderr.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	saved := os.Stderr
	os.Stderr = stderr
	defer func() { os.Stderr = saved }()
	logPath := filepath.Join(t.TempDir(), "server.log")
	log, closeLog := NewLogger(slog.New(slog.NewTextHandler(stderr, nil)), FileOptions{Path: logPath, Keep: 1})
	defer closeLog()
	log.Info("healthy-file-marker")
	if !strings.Contains(read(t, logPath), "healthy-file-marker") {
		t.Fatal("failed stderr write prevented the healthy log file from receiving the record")
	}
}
