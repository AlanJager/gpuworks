//go:build linux

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"gpuworks.ai/gpuworks/internal/server/hostidentity"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

// identityFilesystem runs only the hook's actual maintenance protocol on an
// isolated synthetic filesystem root. Its owner models root, as in the
// hostidentity writer's non-root fixtures. The real-root test below checks
// the actual UID and process permission boundary rather than simulating it.
type identityFilesystem struct {
	t      *testing.T
	root   *os.File
	dir    string
	source func() (*os.File, error)
}

func newIdentityFilesystem(t *testing.T) *identityFilesystem {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "systemd"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	src := filepath.Join(t.TempDir(), "running-installer")
	if err := os.WriteFile(src, []byte("synthetic authorized installer v1"), 0o555); err != nil {
		t.Fatal(err)
	}
	return &identityFilesystem{t: t, root: root, dir: dir, source: func() (*os.File, error) { return os.Open(src) }}
}

func (f *identityFilesystem) Run(ctx context.Context, c selfinstall.Command) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.Path != liveHostIdentityInstaller() || !c.Sudo || len(c.Args) < 3 || c.Args[0] != "host-identity" || c.Args[2] != "/systemd" {
		return fmt.Errorf("unexpected identity maintenance command: %+v", c)
	}
	switch c.Args[1] {
	case "--install-helper":
		if len(c.Args) != 5 || c.Args[3] != "--group" {
			return errors.New("invalid install arguments")
		}
		return hostIdentityHelperAt(f.root, c.Args[2], c.Args[4], false, f.source)
	case "--remove-helper":
		if len(c.Args) != 3 {
			return errors.New("invalid uninstall arguments")
		}
		return hostIdentityHelperAt(f.root, c.Args[2], "", true, nil)
	default:
		return errors.New("unsupported identity maintenance command")
	}
}
func (*identityFilesystem) Output(context.Context, selfinstall.Command) (string, error) {
	return "", errors.New("identity maintenance has no output command")
}
func (f *identityFilesystem) layout() selfinstall.Layout {
	return selfinstall.Layout{Dir: "/service-writable", UnitDir: "/systemd"}
}
func (f *identityFilesystem) dropDir() string {
	return filepath.Join(f.dir, "systemd", "gpuworks.service.d")
}
func (f *identityFilesystem) read(name string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dropDir(), name))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func TestHostIdentityHookPublishesProtectedCopyAndPrunesOnlyItsFiles(t *testing.T) {
	linuxHook(t)
	f := newIdentityFilesystem(t)
	hook, ctx := hostIdentityHook{}, context.Background()
	for range 2 {
		if err := hook.Install(ctx, f, f.layout(), "gpuworks"); err != nil {
			t.Fatal(err)
		}
	}
	want := "# Written by gpuworks install: reads the host identity facts only root may read.\n[Service]\nExecStartPre=-+/systemd/gpuworks.service.d/host-identity-exec host-identity --write /run/gpuworks/host-identity.json --group gpuworks\n"
	if got := f.read("host-identity.conf"); got != want {
		t.Fatalf("drop-in = %q", got)
	}
	if got := f.read(hostIdentityExecutable); got != "synthetic authorized installer v1" {
		t.Fatalf("helper = %q", got)
	}
	for name, mode := range map[string]os.FileMode{hostIdentityExecutable: 0o555, "host-identity.conf": 0o644} {
		st, err := os.Lstat(filepath.Join(f.dropDir(), name))
		if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != mode {
			t.Fatalf("%s: %v, %v", name, st, err)
		}
	}
	// A later explicit privileged CLI install refreshes its own protected
	// snapshot. There is no dependency on the writable service main path.
	next := filepath.Join(t.TempDir(), "next-authorized-installer")
	if err := os.WriteFile(next, []byte("synthetic authorized installer v2"), 0o555); err != nil {
		t.Fatal(err)
	}
	f.source = func() (*os.File, error) { return os.Open(next) }
	if err := hook.Install(ctx, f, f.layout(), "root"); err != nil {
		t.Fatal(err)
	}
	if f.read(hostIdentityExecutable) != "synthetic authorized installer v2" || strings.Contains(f.read("host-identity.conf"), "--group") {
		t.Fatal("explicit install did not refresh the protected snapshot and service group")
	}
	if err := os.WriteFile(filepath.Join(f.dropDir(), "other.conf"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "outside-victim")
	if err := os.WriteFile(victim, []byte("keep outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(f.dropDir(), "host-identity.conf")
	if err := os.Remove(conf); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, conf); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := hook.Uninstall(ctx, f, f.layout()); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(f.dropDir())
	if err != nil || len(entries) != 1 || entries[0].Name() != "other.conf" {
		t.Fatalf("uninstall entries = %v, %v", entries, err)
	}
	if got := f.read("other.conf"); got != "keep" {
		t.Fatal(got)
	}
	if data, err := os.ReadFile(victim); err != nil || string(data) != "keep outside" {
		t.Fatalf("uninstall followed the final symlink: %q, %v", data, err)
	}
}

func TestHostIdentityHookRejectsUntrustedDirectoriesAndSymlinks(t *testing.T) {
	linuxHook(t)
	for _, kind := range []string{"writable ancestor", "symlink ancestor", "writable drop-in directory", "symlink drop-in directory", "helper symlink", "conf symlink", "writable helper", "ancestor ACL"} {
		t.Run(kind, func(t *testing.T) {
			f := newIdentityFilesystem(t)
			outside := filepath.Join(t.TempDir(), "victim")
			if err := os.WriteFile(outside, []byte("untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(f.dropDir(), 0o755); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "writable ancestor":
				if err := os.Chmod(filepath.Join(f.dir, "systemd"), 0o775); err != nil {
					t.Fatal(err)
				}
			case "symlink ancestor":
				if err := os.Rename(filepath.Join(f.dir, "systemd"), filepath.Join(f.dir, "real")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("real", filepath.Join(f.dir, "systemd")); err != nil {
					t.Fatal(err)
				}
			case "writable drop-in directory":
				if err := os.Chmod(f.dropDir(), 0o777); err != nil {
					t.Fatal(err)
				}
			case "symlink drop-in directory":
				if err := os.Remove(f.dropDir()); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(outside), f.dropDir()); err != nil {
					t.Fatal(err)
				}
			case "helper symlink", "conf symlink":
				name := hostIdentityExecutable
				if kind == "conf symlink" {
					name = "host-identity.conf"
				}
				if err := os.Symlink(outside, filepath.Join(f.dropDir(), name)); err != nil {
					t.Fatal(err)
				}
			case "writable helper":
				if err := os.WriteFile(filepath.Join(f.dropDir(), hostIdentityExecutable), []byte("old"), 0o666); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Join(f.dropDir(), hostIdentityExecutable), 0o666); err != nil {
					t.Fatal(err)
				}
			case "ancestor ACL":
				identityTestACL(t, filepath.Join(f.dir, "systemd"))
			}
			f.source = func() (*os.File, error) {
				t.Error("untrusted target reached source copy")
				return nil, errors.New("must not copy")
			}
			if err := (hostIdentityHook{}).Install(context.Background(), f, f.layout(), "gpuworks"); err == nil {
				t.Fatal("untrusted target accepted")
			}
			data, err := os.ReadFile(outside)
			if err != nil || string(data) != "untouched" {
				t.Fatalf("outside victim = %q, %v", data, err)
			}
		})
	}
}

func identityTestACL(t *testing.T, path string) {
	t.Helper()
	// Named user grant with a zero mask: the mode alone appears safe but the
	// extended ACL must still be rejected, including latent masked grants.
	data := make([]byte, 4+5*8)
	binary.LittleEndian.PutUint32(data, 2)
	for n, entry := range [][3]uint32{{1, 7, ^uint32(0)}, {2, 7, uint32(os.Getuid() + 1)}, {4, 0, ^uint32(0)}, {16, 0, ^uint32(0)}, {32, 5, ^uint32(0)}} {
		off := 4 + n*8
		binary.LittleEndian.PutUint16(data[off:], uint16(entry[0]))
		binary.LittleEndian.PutUint16(data[off+2:], uint16(entry[1]))
		binary.LittleEndian.PutUint32(data[off+4:], entry[2])
	}
	if err := unix.Setxattr(path, "system.posix_acl_access", data, 0); err != nil {
		if errors.Is(err, unix.EOPNOTSUPP) && os.Getenv("GPUWORKS_TEST_STRICT") != "1" {
			t.Skip("test filesystem has no POSIX ACL support")
		}
		t.Fatal(err)
	}
}

func TestHostIdentityHookFailedCopyPreservesPreviousPublication(t *testing.T) {
	linuxHook(t)
	f := newIdentityFilesystem(t)
	hook := hostIdentityHook{}
	if err := hook.Install(context.Background(), f, f.layout(), "gpuworks"); err != nil {
		t.Fatal(err)
	}
	oldConf, oldExe := f.read("host-identity.conf"), f.read(hostIdentityExecutable)
	// A real regular file descriptor that cannot be read fails during copy,
	// after temporary creation, rather than simulating publication success.
	bad := filepath.Join(t.TempDir(), "write-only-source")
	if err := os.WriteFile(bad, []byte("must not publish"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.source = func() (*os.File, error) { return os.OpenFile(bad, os.O_WRONLY, 0) }
	if err := hook.Install(context.Background(), f, f.layout(), "root"); err == nil {
		t.Fatal("unreadable source succeeded")
	}
	if f.read("host-identity.conf") != oldConf || f.read(hostIdentityExecutable) != oldExe {
		t.Fatal("failed copy replaced previous publication")
	}
	entries, err := os.ReadDir(f.dropDir())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"host-identity-exec", "host-identity.conf"}) {
		t.Fatalf("temporary files leaked: %v", names)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := hook.Install(ctx, f, f.layout(), "root"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled install = %v", err)
	}
}

func TestHostIdentityHelperRefusesNonRootBeforeMutation(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("covered by real-root permission checks below")
	}
	dir := t.TempDir()
	if err := runHostIdentity([]string{"--install-helper", dir}, io.Discard); !errors.Is(err, hostidentity.ErrNotRoot) {
		t.Fatalf("non-root install = %v", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("non-root install mutated target: %v, %v", entries, err)
	}
}

// This CI-qualified root test uses only its unique directory under /run.
// /tmp is intentionally not used: its writable ancestor must be refused by
// the production trust walk. No systemd operation or host facts write runs.
func TestHostIdentityHelperAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal("strict root helper regression requires root")
		}
		t.Skip("requires root; the strict CI job runs it explicitly")
	}
	dir, err := os.MkdirTemp("/run", "gpuworks-host-identity-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	unitDir := filepath.Join(dir, "systemd")
	if err := os.Mkdir(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"--install-helper", unitDir, "--group", "root"}
	if err := runHostIdentity(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	drop := filepath.Join(unitDir, "gpuworks.service.d")
	helper := filepath.Join(drop, hostIdentityExecutable)
	for path, mode := range map[string]os.FileMode{helper: 0o555, filepath.Join(drop, "host-identity.conf"): 0o644} {
		st, err := os.Lstat(path)
		if err != nil || st.Mode().Perm() != mode || st.Sys().(*syscall.Stat_t).Uid != 0 {
			t.Fatalf("root publication %s: %v, %v", path, st, err)
		}
	}
	digest := func(path string) [32]byte {
		t.Helper()
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			t.Fatal(err)
		}
		var out [32]byte
		copy(out[:], h.Sum(nil))
		return out
	}
	before := digest(helper)
	if before != digest("/proc/self/exe") {
		t.Fatal("helper is not the running installer inode's executable bytes")
	}
	mainDir := filepath.Join(dir, "service-install")
	if err := os.Mkdir(mainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(mainDir, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(mainDir, "gpuworks")
	if err := os.WriteFile(main, []byte("old main"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(main, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	// Exercise the real non-root process boundary, with no supplementary
	// groups. Console-style main writes remain possible; helper writes and
	// replacement are refused by the kernel, independently of Landlock.
	script := `set -eu
	printf 'console replacement' > "$1.new"
	mv "$1.new" "$1"
if (printf x > "$2") 2>/dev/null; then exit 11; fi
if mv "$1" "$2" 2>/dev/null; then exit 12; fi
`
	cmd := exec.Command("/bin/sh", "-c", script, "identity-permissions", main, helper)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534, Groups: []uint32{}}}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("service process permissions: %v: %s", err, out)
	}
	if before != digest(helper) {
		t.Fatal("service main replacement changed root helper")
	}
	if data, _ := os.ReadFile(main); string(data) != "console replacement" {
		t.Fatal("main binary ownership no longer permits console writes")
	}
	// A wrong-owner ancestor must refuse before any replacement.
	if err := os.Chown(unitDir, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	if err := runHostIdentity(args, io.Discard); err == nil {
		t.Fatal("service-owned ancestor accepted")
	}
	if before != digest(helper) {
		t.Fatal("rejected ancestor changed helper")
	}
	if err := os.Chown(unitDir, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := runHostIdentity([]string{"--remove-helper", unitDir}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(helper); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("helper not removed: %v", err)
	}
}
