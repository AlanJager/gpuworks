package expose

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The CLI and the server are not necessarily the same account: the CLI may run
// as root while the server runs as its service user. What makes that work is
// the group on the state directories and the setgid bit, because they decide
// the group of every file either account creates in them.

// TestStoreDirectoriesAreGroupShared: group-writable *and* setgid. The setgid
// bit is the part that is easy to lose — a numeric mode such as 0o2775 does not
// carry it in Go, where permissions come from ModePerm and the bit has to be
// asked for as os.ModeSetgid.
func TestStoreDirectoriesAreGroupShared(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{store.Root(), filepath.Join(store.Root(), "logs"), store.CredentialsDir(), store.BinDir()} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o070 != 0o070 {
			t.Errorf("%s mode = %v, want group read/write/execute", dir, info.Mode().Perm())
		}
		if info.Mode()&os.ModeSetgid == 0 {
			t.Errorf("%s mode = %v, want the setgid bit: without it a file the CLI writes belongs to the CLI's group, and the server cannot read it", dir, info.Mode())
		}
	}
}

// TestManagedFilesShareTheDirectoryGroup: a file that inherits the state
// directory's group is readable by whichever account runs the server, and the
// token is the file that needs it.
func TestManagedFilesShareTheDirectoryGroup(t *testing.T) {
	store := NewStore(t.TempDir())
	tokenFile, err := store.WriteToken("token-value")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.Stat(store.CredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Stat(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := groupOf(t, file), groupOf(t, dir); got != want {
		t.Errorf("token file group = %d, want the credentials directory's group %d", got, want)
	}
	if err := store.WriteConfig(&Config{Revision: "rev-1", Provider: ProviderQuick, Desired: DesiredEnabled}); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.ReadConfig()
	if err != nil || cfg.Revision != "rev-1" {
		t.Fatalf("config round trip: %+v, %v", cfg, err)
	}
}

// groupOf reads the owning group from the file's stat, which is what the
// cross-account read depends on.
func groupOf(t *testing.T, info os.FileInfo) uint32 {
	t.Helper()
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no stat_t on this platform")
	}
	return stat.Gid
}
