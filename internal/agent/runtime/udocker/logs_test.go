package udocker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func writeContainerLog(t *testing.T, d *Driver, name string, lines int) {
	t.Helper()
	if err := os.MkdirAll(d.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	if err := os.WriteFile(d.LogPath(name), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A non-follow read with a tail returns the end of the output. It used to
// return the whole file, and readers that cap what they read got the start of
// a long log instead of the failure at its end.
func TestLogsWithoutFollowReturnsTheLastLines(t *testing.T) {
	d := New(t.TempDir())
	writeContainerLog(t, d, "long", 500)

	rc, err := d.Logs(context.Background(), "long", false, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil || string(got) != "line 498\nline 499\nline 500\n" {
		t.Fatalf("tail 3 = %q err=%v", got, err)
	}

	rc, err = d.Logs(context.Background(), "long", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	all, err := io.ReadAll(rc)
	if err != nil || strings.Count(string(all), "\n") != 500 {
		t.Fatalf("tail 0 returned %d lines err=%v, want the whole file", strings.Count(string(all), "\n"), err)
	}
}

func TestLogsWithoutFollowOfAMissingContainerFails(t *testing.T) {
	d := New(t.TempDir())
	for _, tail := range []int{0, 200} {
		if rc, err := d.Logs(context.Background(), "gone", false, tail); err == nil || !os.IsNotExist(err) {
			if rc != nil {
				rc.Close()
			}
			t.Fatalf("tail %d: err = %v, want not-exist", tail, err)
		}
	}
}

// The follower outlives the removal of the file it follows. tail's complaint
// about that is not container output and must not reach the stream.
func TestFollowedLogsCarryOnlyContainerOutput(t *testing.T) {
	d := New(t.TempDir())
	writeContainerLog(t, d, "removed", 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc, err := d.Logs(ctx, "removed", true, 50)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(rc)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	for _, want := range []string{"line 1", "line 2"} {
		select {
		case got := <-lines:
			if got != want {
				t.Fatalf("followed %q, want %q", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("no %q from the follower", want)
		}
	}
	if err := os.Remove(d.LogPath("removed")); err != nil {
		t.Fatal(err)
	}
	select {
	case got, ok := <-lines:
		if ok {
			t.Fatalf("followed %q after the log was removed, want nothing", got)
		}
	case <-time.After(2 * time.Second):
	}
}
