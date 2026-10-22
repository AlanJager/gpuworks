package engine

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
)

const xfsPrjquotaMounts = "" +
	"/dev/root / ext4 rw,relatime 0 0\n" +
	"/dev/sdb /srv xfs rw,relatime,attr2,prjquota 0 0\n" +
	"/dev/sdc /srv/gpuworks xfs rw,relatime,pquota 0 0\n"

// quotaTest swaps in the four facts the quota depends on and records the
// command it would run.
func quotaTest(t *testing.T, fsType int64, fsErr error, mounts string, binary string, binaryErr error) *[][]string {
	t.Helper()
	var calls [][]string
	original := [...]any{kvDiskFilesystemType, kvDiskMountTable, kvDiskQuotaBinary, kvDiskRunQuota}
	t.Cleanup(func() {
		kvDiskFilesystemType = original[0].(func(string) (int64, error))
		kvDiskMountTable = original[1].(func() (io.ReadCloser, error))
		kvDiskQuotaBinary = original[2].(func() (string, error))
		kvDiskRunQuota = original[3].(func(string, []string) (string, error))
	})
	kvDiskFilesystemType = func(string) (int64, error) { return fsType, fsErr }
	kvDiskMountTable = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(mounts)), nil
	}
	kvDiskQuotaBinary = func() (string, error) { return binary, binaryErr }
	kvDiskRunQuota = func(bin string, args []string) (string, error) {
		calls = append(calls, append([]string{bin}, args...))
		return "", nil
	}
	return &calls
}

// The quota is a bonus, so every precondition it does not get is a silent skip
// and the sweeper remains the thing that holds the limit.
func TestKVDiskProjectQuotaSkipsWhenItCannotBeEnforced(t *testing.T) {
	const dir = "/srv/gpuworks/kv-disk/42"
	for name, test := range map[string]struct {
		fsType    int64
		fsErr     error
		mounts    string
		binaryErr error
	}{
		"the directory cannot be inspected": {fsType: xfsSuperMagic, fsErr: os.ErrNotExist, mounts: xfsPrjquotaMounts},
		"the filesystem is not XFS":         {fsType: 0xEF53, mounts: xfsPrjquotaMounts},
		"XFS without a project quota": {
			fsType: xfsSuperMagic,
			mounts: "/dev/sdb /srv xfs rw,relatime,attr2 0 0\n/dev/sdc /srv/gpuworks xfs rw,noquota 0 0\n",
		},
		"xfs_quota is not installed": {fsType: xfsSuperMagic, mounts: xfsPrjquotaMounts, binaryErr: errors.New("not found")},
	} {
		t.Run(name, func(t *testing.T) {
			calls := quotaTest(t, test.fsType, test.fsErr, test.mounts, "/usr/sbin/xfs_quota", test.binaryErr)
			applyKVDiskProjectQuota(slog.New(slog.DiscardHandler), dir, 42, 4096)
			if len(*calls) != 0 {
				t.Fatalf("xfs_quota ran anyway: %v", *calls)
			}
		})
	}
}

// On a directory that can carry one, the project is set on the deepest XFS
// mount that holds it and the hard block limit is the deployment's ceiling.
func TestKVDiskProjectQuotaCommand(t *testing.T) {
	const dir = "/srv/gpuworks/kv-disk/42"
	calls := quotaTest(t, xfsSuperMagic, nil, xfsPrjquotaMounts, "/usr/sbin/xfs_quota", nil)
	applyKVDiskProjectQuota(slog.New(slog.DiscardHandler), dir, 42, 4096)
	if len(*calls) != 1 {
		t.Fatalf("calls = %v, want exactly one", *calls)
	}
	want := []string{
		"/usr/sbin/xfs_quota", "-x",
		"-c", "project -s -p /srv/gpuworks/kv-disk/42 42",
		"-c", "limit -p bhard=4096 42",
		"/srv/gpuworks",
	}
	got := (*calls)[0]
	if len(got) != len(want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv = %v, want %v", got, want)
		}
	}
}

// /proc/self/mounts escapes a space in a path; the mount point handed to
// xfs_quota has to be the real one.
func TestProjectQuotaMountPointDecodesEscapedPaths(t *testing.T) {
	table := "/dev/sdb /srv/my\\040data xfs rw,prjquota 0 0\n"
	point, ok := projectQuotaMountPoint(strings.NewReader(table), "/srv/my data/kv")
	if !ok || point != "/srv/my data" {
		t.Fatalf("projectQuotaMountPoint() = %q, %v", point, ok)
	}
}
