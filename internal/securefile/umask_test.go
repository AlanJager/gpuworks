//go:build linux || darwin

package securefile

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

func TestCreateStaysPrivateAcrossUmasks(t *testing.T) {
	const key = "GPUWORKS_SECUREFILE_TEST_UMASK"
	if raw := os.Getenv(key); raw != "" {
		mask, err := strconv.ParseInt(raw, 8, 32)
		if err != nil {
			t.Fatal(err)
		}
		old := syscall.Umask(int(mask))
		defer syscall.Umask(old)
		group, err := TrustedGroup(os.Getgid())
		if err != nil {
			t.Fatal(err)
		}
		for _, policy := range []Policy{{}, group} {
			path := filepath.Join(t.TempDir(), "secret")
			f, err := Create(path, policy)
			if err != nil {
				t.Fatal(err)
			}
			f.Close()
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != policy.Mode() {
				t.Fatalf("umask%s mode=%#o want%#o", raw, info.Mode().Perm(), policy.Mode())
			}
		}
		return
	}
	for _, mask := range []string{"0000", "0022", "0077"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCreateStaysPrivateAcrossUmasks$")
		cmd.Env = append(os.Environ(), key+"="+mask)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("umask%s: %v\n%s", mask, err, output)
		}
	}
}
