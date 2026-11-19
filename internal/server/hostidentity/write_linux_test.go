//go:build linux

package hostidentity

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// runDir is a directory with the mode Write requires, owned by whoever
// runs the test.
func runDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		t.Fatal(err)
	}
	return dir
}

// self is a target owned by the test's account, so the write runs without
// root.
func self(dir string) target {
	return target{dir: dir, name: "host-identity.json", uid: os.Getuid(), gid: os.Getgid(), dirUID: os.Getuid()}
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range des {
		out = append(out, d.Name())
	}
	return out
}

// The file gets mode 0440 and its owner through its descriptor, and
// replaces the old file by rename: a reader holding the old file still
// reads it whole, the path reads the new one, and no temporary file is
// left behind.
func TestWriteReplacesTheFileAtomically(t *testing.T) {
	dir := runDir(t)
	path := filepath.Join(dir, "host-identity.json")
	if err := os.WriteFile(path, []byte("old facts\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	facts := Facts{V: 1, MachineID: strings.Repeat("ab", 16)}
	if err := writeAt(self(dir), facts.Marshal()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != fileMode {
		t.Fatalf("mode = %v, want %#o", info.Mode(), fileMode)
	}
	if st := info.Sys().(*syscall.Stat_t); int(st.Uid) != os.Getuid() || int(st.Gid) != os.Getgid() {
		t.Fatalf("owner = %d:%d", st.Uid, st.Gid)
	}
	data, _ := os.ReadFile(path)
	if back, err := Parse(data); err != nil || back != facts {
		t.Fatalf("written = %s (%v)", data, err)
	}
	held := make([]byte, 64)
	n, _ := old.Read(held)
	if string(held[:n]) != "old facts\n" {
		t.Fatalf("the old file was rewritten in place: %q", held[:n])
	}
	if got := entries(t, dir); len(got) != 1 || got[0] != "host-identity.json" {
		t.Fatalf("directory holds %v", got)
	}
}

// A write that fails before the rename leaves the old file as it was and
// removes its temporary file.
func TestWriteFailureLeavesTheOldFile(t *testing.T) {
	dir := runDir(t)
	path := filepath.Join(dir, "host-identity.json")
	if err := os.WriteFile(path, []byte("old facts\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	tg := self(dir)
	tg.sync = func(*os.File) error { return errors.New("input/output error") }
	if err := writeAt(tg, []byte("new\n")); err == nil || !strings.Contains(err.Error(), "input/output error") {
		t.Fatalf("err = %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "old facts\n" {
		t.Fatalf("old file = %q", data)
	}
	if got := entries(t, dir); len(got) != 1 {
		t.Fatalf("directory holds %v", got)
	}
}

// A symlink planted at the temporary path is not followed: the create
// fails and the symlink's target is untouched. One planted at the final
// path is replaced, never written through.
func TestWriteNeverFollowsAPlantedSymlink(t *testing.T) {
	dir := runDir(t)
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tg := self(dir)
	tg.tempName = func() string { return ".host-identity-planted.tmp" }
	if err := os.Symlink(victim, filepath.Join(dir, ".host-identity-planted.tmp")); err != nil {
		t.Fatal(err)
	}
	if err := writeAt(tg, []byte("new\n")); err == nil || !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("write through a planted temp = %v", err)
	}
	if data, _ := os.ReadFile(victim); string(data) != "keep\n" {
		t.Fatalf("victim = %q", data)
	}
	if err := os.Remove(filepath.Join(dir, ".host-identity-planted.tmp")); err != nil {
		t.Fatal(err)
	}

	final := filepath.Join(dir, "host-identity.json")
	if err := os.Symlink(victim, final); err != nil {
		t.Fatal(err)
	}
	if err := writeAt(self(dir), []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(final); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("final path = %v, %v", info, err)
	}
	if data, _ := os.ReadFile(victim); string(data) != "keep\n" {
		t.Fatalf("victim = %q", data)
	}
}

// The directory must be owned by the expected account (root in
// production) with mode 0755, and must not be a symlink; a missing one is
// created with that mode.
func TestWriteChecksTheDirectory(t *testing.T) {
	dir := runDir(t)
	other := self(dir)
	other.dirUID = os.Getuid() + 1
	if err := writeAt(other, []byte("x\n")); err == nil || !strings.Contains(err.Error(), "must be owned by uid") {
		t.Fatalf("directory with another owner = %v", err)
	}
	if err := os.Chmod(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := writeAt(self(dir), []byte("x\n")); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("group-writable directory = %v", err)
	}
	if got := entries(t, dir); len(got) != 0 {
		t.Fatalf("a refused directory got %v", got)
	}

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(runDir(t), link); err != nil {
		t.Fatal(err)
	}
	if err := writeAt(self(link), []byte("x\n")); err == nil {
		t.Fatal("wrote through a symlinked directory")
	}

	fresh := filepath.Join(t.TempDir(), "gpuworks")
	old := syscall.Umask(0o077)
	err := writeAt(self(fresh), []byte("x\n"))
	syscall.Umask(old)
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(fresh); info.Mode().Perm() != dirMode {
		t.Fatalf("created directory mode = %v", info.Mode())
	}
}

// Write refuses a relative or unclean path, and an account other than
// root, before it reads or writes anything.
func TestWriteRefusesBadPathsAndNonRoot(t *testing.T) {
	for _, p := range []string{"run/gpuworks/host-identity.json", "/run/gpuworks/../x.json", "/"} {
		if err := Write(p, ""); err == nil || errors.Is(err, ErrNotRoot) {
			t.Errorf("Write(%q) = %v", p, err)
		}
	}
	if os.Geteuid() == 0 {
		return
	}
	path := filepath.Join(runDir(t), "host-identity.json")
	if err := Write(path, ""); !errors.Is(err, ErrNotRoot) {
		t.Fatalf("non-root Write = %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("non-root Write left %v", err)
	}
}

// As root, Write gives the file root:<group> and mode 0440 in a root-owned
// 0755 directory, so the service group reads it and cannot change or
// replace it. It needs root and a group other than root's: it skips
// without them, and fails under GPUWORKS_TEST_STRICT=1, which the core-test
// CI job (root) sets for it.
//
//	sudo go test -run TestWriteAsRootOwnsTheFile ./internal/server/hostidentity
func TestWriteAsRootOwnsTheFile(t *testing.T) {
	if os.Geteuid() != 0 {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal("GPUWORKS_TEST_STRICT=1: this case needs root")
		}
		t.Skip("needs root")
	}
	var group *user.Group
	for _, name := range []string{"nogroup", "nobody", "daemon", "bin"} {
		if g, err := user.LookupGroup(name); err == nil && g.Gid != "0" {
			group = g
			break
		}
	}
	if group == nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal("GPUWORKS_TEST_STRICT=1: no unprivileged group on this host")
		}
		t.Skip("no unprivileged group on this host")
	}
	dir := filepath.Join(t.TempDir(), "gpuworks")
	path := filepath.Join(dir, "host-identity.json")
	if err := Write(path, group.Name); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	if info.Mode() != fileMode || st.Uid != 0 || strconv.Itoa(int(st.Gid)) != group.Gid {
		t.Fatalf("file %v %d:%d, want %#o 0:%s", info.Mode(), st.Uid, st.Gid, fileMode, group.Gid)
	}
	if dinfo, _ := os.Lstat(dir); dinfo.Mode().Perm() != dirMode || dinfo.Sys().(*syscall.Stat_t).Uid != 0 {
		t.Fatalf("directory %v", dinfo.Mode())
	}
	data, _ := os.ReadFile(path)
	if _, err := Parse(data); err != nil {
		t.Fatalf("written file: %v", err)
	}
	if err := Write(path, "no-such-group-gwx"); err == nil {
		t.Fatal("an unknown group was accepted")
	}
}
