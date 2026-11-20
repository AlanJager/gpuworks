//go:build linux || darwin

package filelock

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestIndependentDescriptorsAndIdempotentRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}
	release, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = release() })
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode().Perm() != 0600 {
		t.Fatalf("mode=%v", before.Mode())
	}
	if other, err := Acquire(path); !errors.Is(err, ErrBusy) || other != nil {
		t.Fatalf("second descriptor acquired held lock: %v", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := release(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("release replaced or removed lock inode: %v", err)
	}
	next, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer next()
	if err = release(); err != nil {
		t.Fatal(err)
	}
	if other, err := Acquire(path); !errors.Is(err, ErrBusy) || other != nil {
		t.Fatalf("old release unlocked new ownership: %v", err)
	}
}

func TestAcquireFileFailures(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{filepath.Join(root, "missing", "lock"), root} {
		release, err := Acquire(path)
		if err == nil || release != nil || errors.Is(err, ErrBusy) {
			t.Fatalf("file failure accepted or misclassified: %s %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Acquire created parent: %v", err)
	}
	// Failed opens leave the valid directory usable for later acquisitions.
	release, err := Acquire(filepath.Join(root, "lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err = release(); err != nil {
		t.Fatal(err)
	}
}

// TestFilelockProcessHelper runs only in an explicitly launched test child.
func TestFilelockProcessHelper(t *testing.T) {
	path := os.Getenv("GPUWORKS_FILELOCK_TEST_PATH")
	if path == "" {
		return
	}
	release, err := Acquire(path)
	if errors.Is(err, ErrBusy) {
		fmt.Println("busy")
		os.Exit(23)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(24)
	}
	fmt.Println("locked")
	var signal [1]byte
	_, _ = os.Stdin.Read(signal[:])
	if err = release(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(25)
	}
	os.Exit(0)
}

func lockChild(t *testing.T, path string) (*exec.Cmd, io.WriteCloser, *bufio.Reader) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFilelockProcessHelper$")
	cmd.Env = append(os.Environ(), "GPUWORKS_FILELOCK_TEST_PATH="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, input, bufio.NewReader(stdout)
}

func TestProcessExclusionAndReleaseAfterExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "process.lock")
	release, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = release() })
	child, _, output := lockChild(t, path)
	line, err := output.ReadString('\n')
	if err != nil || line != "busy\n" {
		t.Fatalf("child did not reject parent lock: %q %v", line, err)
	}
	var exit *exec.ExitError
	if err = child.Wait(); !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("expected busy exit: %v", err)
	}
	if err = release(); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"release", "kill"} {
		t.Run(mode, func(t *testing.T) {
			child, input, output := lockChild(t, path)
			line, err := output.ReadString('\n')
			if err != nil || line != "locked\n" {
				t.Fatalf("child acquisition failed: %q %v", line, err)
			}
			if other, err := Acquire(path); !errors.Is(err, ErrBusy) || other != nil {
				t.Fatalf("parent bypassed child lock: %v", err)
			}
			if mode == "kill" {
				if err = child.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				if err = child.Wait(); err == nil {
					t.Fatal("killed child exited successfully")
				}
			} else {
				if err = input.Close(); err != nil {
					t.Fatal(err)
				}
				if err = child.Wait(); err != nil {
					t.Fatal(err)
				}
			}
			next, err := Acquire(path)
			if err != nil {
				t.Fatalf("lock survived child %s: %v", mode, err)
			}
			if err = next(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
