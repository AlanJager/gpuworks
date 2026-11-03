package sqliteutil

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWrapOpenErrorPermissionFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the fixture's Unix permission restrictions")
	}
	for _, mode := range []os.FileMode{0o500, 0o000} {
		t.Run(mode.String(), func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "data")
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(parent, 0o700)
			if err := os.Chmod(parent, mode); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "gpuworks.db")
			err := WrapOpenError(path, cantOpen(t, path))
			want := "write permission"
			if mode == 0 {
				want = "permission denied"
			}
			if !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "out of memory") {
				t.Fatalf("unhelpful permission diagnostic: %v", err)
			}
		})
	}
}

func TestWrapOpenErrorDoesNotOpenSpecialFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- WrapOpenError(path, &codeError{14, "cannot open"}) }()
	select {
	case err := <-done:
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("missing special file diagnostic: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("diagnostic blocked while inspecting a FIFO")
	}
}
