package udocker

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// TestMain points TMPDIR at a scratch directory so no test in this package —
// whatever driver constructor it uses — leaves short-path /tmp sources in the
// host's real /tmp. The scratch root stays traversable like /tmp itself:
// under root, fixtures run the udocker CLI as a dedicated user and only open
// up their own t.TempDir levels (makeUdockerTempRootTraversable).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "udocker-test-tmp-")
	if err == nil {
		err = os.Chmod(dir, 0o755)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = os.Setenv("TMPDIR", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func tmpBind(mounts []runtime.Mount) string {
	for _, m := range mounts {
		if filepath.Clean(m.Target) == "/tmp" {
			return m.Source
		}
	}
	return ""
}

// TestWorkloadMountsHasNoFilesystemSideEffects pins that building a
// workload's binds — which terminals and exec do too — never creates the /tmp
// source; only the launch path does.
func TestWorkloadMountsHasNoFilesystemSideEffects(t *testing.T) {
	d := runveilDriver(t)
	d.hostTmpRoot = t.TempDir()
	spec := runtime.WorkloadSpec{Name: "demo", Env: map[string]string{}}
	if got := tmpBind(d.workloadMounts(spec)); got != "" {
		t.Fatalf("unprepared workload got a /tmp bind %q", got)
	}
	if _, err := os.Lstat(d.workloadTmpParent()); !os.IsNotExist(err) {
		t.Fatalf("workloadMounts created %s: %v", d.workloadTmpParent(), err)
	}
}

// TestEnsureWorkloadTmpBindsPrivateShortPath pins the sun_path budget fix:
// guest /tmp resolves to a short host path so an engine's IPC socket (36-char
// tempfile name appended) still fits sockaddr_un's 108 bytes. Before the bind
// PRoot translated /tmp onto UDOCKER_DIR/containers/<name>/ROOT/tmp and vLLM's
// ZMQ died with "File name too long" on deep state directories.
func TestEnsureWorkloadTmpBindsPrivateShortPath(t *testing.T) {
	d := runveilDriver(t)
	d.hostTmpRoot = t.TempDir()
	name := "gpuworks-56faadeed172"
	dir, err := d.ensureWorkloadTmp(name)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(d.hostTmpRoot, "gpuworks-udocker-"+strconv.Itoa(os.Geteuid()))
	if dir != filepath.Join(parent, name) {
		t.Fatalf("/tmp source = %q, want under %q", dir, parent)
	}
	assertMode(t, parent, 0o711)
	assertMode(t, dir, 0o700)
	var st syscall.Stat_t
	if err := syscall.Lstat(dir, &st); err != nil || int(st.Uid) != os.Geteuid() {
		t.Fatalf("/tmp source owner = %d (err %v), want the agent uid %d", st.Uid, err, os.Geteuid())
	}
	spec := runtime.WorkloadSpec{Name: name, Env: map[string]string{"HYGON_VISIBLE_DEVICES": "0"}}
	if got := tmpBind(d.workloadMounts(spec)); got != dir {
		t.Fatalf("/tmp bind = %q, want %q", got, dir)
	}
	// Repeat launches of the same workload reuse the directory.
	if again, err := d.ensureWorkloadTmp(name); err != nil || again != dir {
		t.Fatalf("second ensure = %q, %v", again, err)
	}
	// A caller-provided /tmp mount wins over the generated bind.
	caller := t.TempDir()
	spec.Mounts = []runtime.Mount{{Source: caller, Target: "/tmp"}}
	if got := tmpBind(d.workloadMounts(spec)); got != caller {
		t.Fatalf("caller /tmp mount overridden by %q", got)
	}
	// The budget holds for the production default root (/tmp, no override).
	t.Setenv("TMPDIR", "/tmp")
	fresh := New(t.TempDir())
	source, err := fresh.workloadTmpDir(name)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(source) + 1 + 36; got > 108 {
		t.Fatalf("default /tmp source %q leaves %d bytes of socket path, over the 108-byte limit", source, got)
	}
}

// TestEnsureWorkloadTmpRestoresParentMode covers a parent created under a
// restrictive umask: the run user must still be able to traverse it.
func TestEnsureWorkloadTmpRestoresParentMode(t *testing.T) {
	d := runveilDriver(t)
	d.hostTmpRoot = t.TempDir()
	if err := os.Mkdir(d.workloadTmpParent(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ensureWorkloadTmp("demo"); err != nil {
		t.Fatal(err)
	}
	assertMode(t, d.workloadTmpParent(), 0o711)
}

// TestEnsureWorkloadTmpRefusesUntrustedParent pins the shared-/tmp hardening:
// a parent another user could have planted (writable by others, or a
// symlink) disables the bind instead of being written through.
func TestEnsureWorkloadTmpRefusesUntrustedParent(t *testing.T) {
	cases := map[string]func(t *testing.T, parent string){
		"world writable": func(t *testing.T, parent string) {
			if err := os.Mkdir(parent, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(parent, 0o777); err != nil {
				t.Fatal(err)
			}
		},
		"symlink": func(t *testing.T, parent string) {
			if err := os.Symlink(t.TempDir(), parent); err != nil {
				t.Fatal(err)
			}
		},
		"regular file": func(t *testing.T, parent string) {
			if err := os.WriteFile(parent, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			d := runveilDriver(t)
			d.hostTmpRoot = t.TempDir()
			parent := d.workloadTmpParent()
			plant(t, parent)
			if dir, err := d.ensureWorkloadTmp("demo"); err == nil {
				t.Fatalf("untrusted parent accepted, /tmp source %q", dir)
			}
			if _, err := os.Lstat(filepath.Join(parent, "demo")); err == nil {
				t.Fatal("container dir created under an untrusted parent")
			}
			if got := tmpBind(d.workloadMounts(runtime.WorkloadSpec{Name: "demo", Env: map[string]string{}})); got != "" {
				t.Fatalf("untrusted parent still bound as /tmp: %q", got)
			}
		})
	}
}

func TestEnsureWorkloadTmpRejectsInvalidNames(t *testing.T) {
	d := runveilDriver(t)
	d.hostTmpRoot = t.TempDir()
	for _, name := range []string{"", ".", "..", "a/b", "../escape"} {
		if dir, err := d.ensureWorkloadTmp(name); err == nil {
			t.Fatalf("name %q accepted as %q", name, dir)
		}
	}
}

// TestEnsureWorkloadTmpChownsToWorkloadUser needs root: only a root agent runs
// workloads as a different user, and only root can chown to it.
func TestEnsureWorkloadTmpChownsToWorkloadUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown to another uid requires root")
	}
	d := runveilDriver(t)
	d.hostTmpRoot = t.TempDir()
	d.runUser, d.runUID, d.runGID = "gpuworks-test", 54321, 54322
	dir, err := d.ensureWorkloadTmp("demo")
	if err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(dir, &st); err != nil || st.Uid != 54321 || st.Gid != 54322 {
		t.Fatalf("/tmp source owner = %d:%d (err %v), want 54321:54322", st.Uid, st.Gid, err)
	}
	assertMode(t, dir, 0o700)
}

func TestRemoveContainerDirCleansShortPathTmp(t *testing.T) {
	d := runveilDriver(t)
	d.hostTmpRoot = t.TempDir()
	runveilContainer(t, d, "demo")
	source, err := d.ensureWorkloadTmp("demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.removeContainerDir("demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("short-path tmp dir survived teardown: %v", err)
	}
}

// TestRemoveWorkloadTmpLeavesUntrustedParentAlone pins that teardown never
// deletes through a parent the agent does not own.
func TestRemoveWorkloadTmpLeavesUntrustedParentAlone(t *testing.T) {
	d := runveilDriver(t)
	d.hostTmpRoot = t.TempDir()
	elsewhere := t.TempDir()
	victim := filepath.Join(elsewhere, "demo")
	if err := os.Mkdir(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, d.workloadTmpParent()); err != nil {
		t.Fatal(err)
	}
	d.removeWorkloadTmp("demo")
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("teardown deleted through a symlinked parent: %v", err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want || info.Mode()&(os.ModeSticky|os.ModeSymlink) != 0 {
		t.Fatalf("%s mode = %v, want %v", path, info.Mode(), want)
	}
}
