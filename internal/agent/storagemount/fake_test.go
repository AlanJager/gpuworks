package storagemount

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// exitErr mimics *exec.ExitError for a command that ran and failed.
type exitErr struct{ code int }

func (e exitErr) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// fakeExport describes how one NFS export behaves when mounted.
type fakeExport struct {
	failOutput string // non-empty: mount fails with exit 32 and this stderr
	readOnly   bool   // the mounted tree rejects writes
	vers       string // negotiated version, default 4.2
}

// fakeHost is a faithful stand-in for the mount and umount commands. It validates the argument
// shape the real mount(8) would accept, and keeps a mountinfo file in sync with what is mounted.
type fakeHost struct {
	t        *testing.T
	mu       sync.Mutex
	exports  map[string]fakeExport
	busy     map[string]bool // mount points umount refuses with "target is busy"
	mounts   []fakeMount
	calls    [][]string
	infoPath string
	nextID   int

	mountHook func(ctx context.Context) // runs before mount decides; used to block
}

type fakeMount struct{ path, export, opts string }

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	h := &fakeHost{t: t, exports: map[string]fakeExport{}, busy: map[string]bool{}, nextID: 40}
	h.infoPath = filepath.Join(t.TempDir(), "mountinfo")
	h.writeInfo()
	return h
}

func (h *fakeHost) escape(s string) string {
	return strings.NewReplacer(`\`, `\134`, " ", `\040`, "\t", `\011`, "\n", `\012`).Replace(s)
}

func (h *fakeHost) writeInfo() {
	var b strings.Builder
	b.WriteString("22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw,errors=remount-ro\n")
	b.WriteString("23 22 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw\n")
	for i, m := range h.mounts {
		exp := h.exports[m.export]
		vers := exp.vers
		if vers == "" {
			vers = "4.2"
		}
		mode := "rw"
		if exp.readOnly {
			mode = "ro"
		}
		fmt.Fprintf(&b, "%d 22 0:%d / %s %s,noatime shared:%d - nfs4 %s %s,vers=%s,rsize=1048576,proto=tcp\n",
			100+i, 50+i, h.escape(m.path), mode, 200+i, m.export, mode, vers)
	}
	if err := os.WriteFile(h.infoPath, []byte(b.String()), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// addForeign registers a mount that this agent did not create.
func (h *fakeHost) addForeign(path, export string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mounts = append(h.mounts, fakeMount{path: path, export: export})
	if _, ok := h.exports[export]; !ok {
		h.exports[export] = fakeExport{}
	}
	h.writeInfo()
}

func (h *fakeHost) mounted(path string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.mounts {
		if m.path == path {
			return true
		}
	}
	return false
}

func (h *fakeHost) callsTo(name string) [][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out [][]string
	for _, c := range h.calls {
		if c[0] == name {
			out = append(out, c)
		}
	}
	return out
}

func (h *fakeHost) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	h.mu.Lock()
	h.calls = append(h.calls, append([]string{name}, args...))
	hook := h.mountHook
	h.mu.Unlock()
	switch name {
	case "mount":
		if hook != nil {
			hook(ctx)
		}
		return h.mount(args)
	case "umount":
		return h.umount(args)
	}
	return []byte(name + ": command not found\n"), exitErr{127}
}

func (h *fakeHost) mount(args []string) ([]byte, error) {
	// mount -t nfs -o <opts> <export> <path>, exactly as the code under test must call it.
	if len(args) != 6 || args[0] != "-t" || args[1] != "nfs" || args[2] != "-o" {
		return []byte("Usage: mount [-lhV] ...\n"), exitErr{1}
	}
	opts, export, path := args[3], args[4], args[5]
	if strings.ContainsAny(opts, " ;$`") {
		return []byte("mount.nfs: an incorrect mount option was specified\n"), exitErr{32}
	}
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		return []byte(fmt.Sprintf("mount.nfs: mount point %s does not exist\n", path)), exitErr{32}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	exp, ok := h.exports[export]
	if !ok {
		return []byte(fmt.Sprintf("mount.nfs: mounting %s failed, reason given by server: No such file or directory\n", export)), exitErr{32}
	}
	if exp.failOutput != "" {
		return []byte(exp.failOutput + "\n"), exitErr{32}
	}
	for _, m := range h.mounts {
		if m.path == path {
			// The kernel would stack the mount; the code under test must never get here.
			h.t.Errorf("fake mount: %s already has a mount stacked on it", path)
		}
	}
	if exp.readOnly {
		// Writes to a read-only tree fail; model that by occupying the probe directory name with a file.
		if err := os.WriteFile(filepath.Join(path, MetaDirName), nil, 0o444); err != nil {
			h.t.Fatal(err)
		}
	}
	h.mounts = append(h.mounts, fakeMount{path: path, export: export, opts: opts})
	h.writeInfo()
	return nil, nil
}

func (h *fakeHost) umount(args []string) ([]byte, error) {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		h.t.Errorf("umount must be called as plain `umount <path>`, got %v", args)
		return []byte("umount: bad usage\n"), exitErr{1}
	}
	path := args[0]
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, m := range h.mounts {
		if m.path != path {
			continue
		}
		if h.busy[path] {
			return []byte(fmt.Sprintf("umount: %s: target is busy.\n", path)), exitErr{32}
		}
		h.mounts = append(h.mounts[:i], h.mounts[i+1:]...)
		h.writeInfo()
		// The fake has no real filesystem behind the mount: what was written "through" it lands in the
		// mount point directory. A real unmount uncovers the empty directory, so clear it.
		ents, _ := os.ReadDir(path)
		for _, x := range ents {
			os.RemoveAll(filepath.Join(path, x.Name()))
		}
		return nil, nil
	}
	return []byte(fmt.Sprintf("umount: %s: not mounted.\n", path)), exitErr{32}
}

// fakeStatfs returns canned capacity and can block chosen paths until released.
type fakeStatfs struct {
	mu      sync.Mutex
	calls   map[string]int
	block   map[string]chan struct{}
	failFor map[string]error
	fsType  map[string]uint64
}

func newFakeStatfs() *fakeStatfs {
	return &fakeStatfs{calls: map[string]int{}, block: map[string]chan struct{}{}, failFor: map[string]error{}, fsType: map[string]uint64{}}
}

func (f *fakeStatfs) blockPath(p string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan struct{})
	f.block[p] = ch
	return ch
}

func (f *fakeStatfs) count(p string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[p]
}

func (f *fakeStatfs) fn(path string) (Usage, error) {
	f.mu.Lock()
	f.calls[path]++
	ch := f.block[path]
	err := f.failFor[path]
	f.mu.Unlock()
	if ch != nil {
		<-ch
	}
	if err != nil {
		return Usage{}, err
	}
	f.mu.Lock()
	magic, ok := f.fsType[path]
	f.mu.Unlock()
	if !ok {
		magic = 0x6969 // NFS_SUPER_MAGIC, unless a test says the path is on the local disk
	}
	return Usage{TotalBytes: 100 << 30, AvailableBytes: 40 << 30, FSType: magic}, nil
}

// onLocalDisk makes statfs report an ext4 filesystem for path.
func (f *fakeStatfs) onLocalDisk(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fsType[path] = 0xEF53
}

type env struct {
	h    *fakeHost
	sf   *fakeStatfs
	root bool
	dir  string
	busy func(string) bool
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return &env{h: newFakeHost(t), sf: newFakeStatfs(), root: true, dir: t.TempDir()}
}

func (e *env) manager(mut ...func(*Options)) *Manager {
	o := Options{
		Runner:        e.h.run,
		MountinfoPath: e.h.infoPath,
		Statfs:        e.sf.fn,
		IsRoot:        func() bool { return e.root },
		CallTimeout:   300 * time.Millisecond,
		MountTimeout:  time.Second,
		ProbeDir:      filepath.Join(e.dir, "probe"),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if e.busy != nil {
		o.Busy = e.busy
	}
	for _, f := range mut {
		f(&o)
	}
	return New(o)
}

func (e *env) path(name string) string { return filepath.Join(e.dir, name) }

func (e *env) nfsSpec(id uint, name, export string) Spec {
	return Spec{ID: id, Name: name, Type: TypeNFS, Path: e.path(name), Export: export}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (h *fakeHost) mountList() []fakeMount {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]fakeMount(nil), h.mounts...)
}
