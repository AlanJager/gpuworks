package perftest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeLocator struct {
	hostErr    error
	extractErr error
	stdout     string
	stderr     string
	versionErr error
	extracted  int
	versions   int
}

func (f *fakeLocator) locator(t *testing.T, clock *time.Time) *locator {
	dir := t.TempDir()
	return &locator{
		dir: dir,
		lookPath: func(name string) (string, error) {
			if name != Tool {
				t.Fatalf("looked up %q", name)
			}
			if f.hostErr != nil {
				return "", f.hostErr
			}
			return "/usr/bin/ib_write_bw", nil
		},
		extract: func(dest string) (string, error) {
			f.extracted++
			if dest != filepath.Join(dir, Tool) {
				t.Fatalf("extract dest = %q", dest)
			}
			return dest, f.extractErr
		},
		version: func(_ context.Context, bin string) ([]byte, []byte, error) {
			f.versions++
			return []byte(f.stdout), []byte(f.stderr), f.versionErr
		},
		now: func() time.Time { return *clock },
	}
}

func TestLocatePrefersHostTool(t *testing.T) {
	clock := time.Unix(0, 0)
	f := &fakeLocator{}
	got, err := f.locator(t, &clock).locate()
	if err != nil || got != "/usr/bin/ib_write_bw" || f.extracted != 0 {
		t.Fatalf("locate = %q, %v (extracted %d)", got, err, f.extracted)
	}
}

func TestLocateFallsBackToRunnableEmbeddedTool(t *testing.T) {
	clock := time.Unix(0, 0)
	// ib_write_bw --version exits 1 even when it works.
	f := &fakeLocator{hostErr: errors.New("not found"), stdout: "Version: 6.10\n", versionErr: errors.New("exit status 1")}
	l := f.locator(t, &clock)
	got, err := l.locate()
	if err != nil || got != filepath.Join(l.dir, Tool) {
		t.Fatalf("locate = %q, %v", got, err)
	}
	if _, err := l.locate(); err != nil || f.versions != 1 {
		t.Fatalf("second locate err %v, version checks %d (want cached)", err, f.versions)
	}
}

func TestLocateReportsWhyEmbeddedToolCannotRun(t *testing.T) {
	clock := time.Unix(0, 0)
	f := &fakeLocator{
		hostErr:    errors.New("not found"),
		stderr:     "/var/lib/gpuworks/tools/ib_write_bw: error while loading shared libraries: librdmacm.so.1: cannot open shared object file: No such file or directory\n",
		versionErr: errors.New("exit status 127"),
	}
	l := f.locator(t, &clock)
	_, err := l.locate()
	if !errors.Is(err, ErrToolMissing) || !strings.Contains(err.Error(), "install perftest") || !strings.Contains(err.Error(), "librdmacm.so.1") {
		t.Fatalf("err = %v", err)
	}
	// A failure is cached for a while, then retried: installing the RDMA
	// libraries fixes it without an agent restart.
	clock = clock.Add(time.Minute)
	if _, err := l.locate(); err == nil || f.versions != 1 {
		t.Fatalf("retried too soon: err %v, version checks %d", err, f.versions)
	}
	clock = clock.Add(recheckAfter)
	f.stdout, f.stderr, f.versionErr = "Version: 6.10\n", "", nil
	if _, err := l.locate(); err != nil || f.versions != 2 {
		t.Fatalf("recheck: err %v, version checks %d", err, f.versions)
	}
}

func TestLocateWithoutEmbeddedTool(t *testing.T) {
	clock := time.Unix(0, 0)
	f := &fakeLocator{hostErr: errors.New("not found"), extractErr: errors.New("no embedded ib_write_bw for riscv64")}
	_, err := f.locator(t, &clock).locate()
	if !errors.Is(err, ErrToolMissing) || !strings.Contains(err.Error(), "riscv64") || f.versions != 0 {
		t.Fatalf("err = %v, version checks %d", err, f.versions)
	}
}

func TestLocateEmptyVersionOutputFallsBackToExitError(t *testing.T) {
	clock := time.Unix(0, 0)
	f := &fakeLocator{hostErr: errors.New("not found"), versionErr: context.DeadlineExceeded}
	_, err := f.locator(t, &clock).locate()
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("err = %v", err)
	}
}

// Host runs the real tool on PATH, reading its stdout and stderr apart; a
// fake ib_write_bw stands in for perftest.
func TestHostRunsToolOnPath(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case " $* " in
*" 10.0.0.9 "*) echo "---------------------------------------------------------------------------------------"; echo "Failed to modify QP 7 to RTR" >&2; exit 1 ;;
*" 10.0.0.2 "*) printf ' #bytes #iterations BW peak[Gb/sec] BW average[Gb/sec] MsgRate[Mpps]\n 65536 100 0.00 24.50 0.04\n' ;;
*) sleep 30 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, Tool), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	r := Host(t.TempDir())
	r.SysRoot = ""
	bin, err := r.Locate()
	if err != nil || bin != filepath.Join(dir, Tool) {
		t.Fatalf("locate = %q, %v", bin, err)
	}
	rails := []Rail{{Device: "mlx5_0", Port: 1}}
	if got := r.Run(context.Background(), "10.0.0.2", rails, 18515, 1); got[0].Gbps != 24.5 || got[0].Code != "" {
		t.Fatalf("run = %+v", got)
	}
	got := r.Run(context.Background(), "10.0.0.9", rails, 18515, 1)
	if got[0].Error != "Failed to modify QP 7 to RTR (exit status 1)" {
		t.Fatalf("failure = %+v", got)
	}
	// A served rail keeps running in the background until its context ends.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if failed := r.Serve(ctx, rails, 18515, 1); len(failed) != 0 {
		t.Fatalf("serve = %+v", failed)
	}
}

func TestNewLocatorWiresTheHost(t *testing.T) {
	l := newLocator(t.TempDir())
	if l.lookPath == nil || l.extract == nil || l.version == nil || l.now == nil {
		t.Fatal("locator not wired")
	}
	// The embedded tool for this architecture extracts and answers --version
	// only where libibverbs is installed; either way the answer is definite.
	t.Setenv("PATH", t.TempDir())
	if _, err := l.locate(); err != nil && !errors.Is(err, ErrToolMissing) {
		t.Fatalf("locate err = %v", err)
	}
	if (&missingError{}).Error() != ErrToolMissing.Error() {
		t.Fatal("empty detail")
	}
}
