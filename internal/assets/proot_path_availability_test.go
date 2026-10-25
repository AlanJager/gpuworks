//go:build linux

package assets

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// A per-process seccomp filter reproduces old-kernel ENOSYS without changing
// the host or relaxing the PRoot gate. It also distinguishes host limitations
// from a regression that makes only guest calls fail.
func testGuestPathGateSyscallAvailability(t *testing.T, cc, dir, proot, binary, source, guest string) {
	t.Helper()
	code := `#define _GNU_SOURCE
#include <errno.h>
#include <stddef.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>
#include <sys/prctl.h>
#include <linux/filter.h>
#include <linux/seccomp.h>
int main(int argc, char **argv) {
 if (argc < 3) return 2;
 int error = atoi(argv[1]);
 if (error != ENOSYS && error != EPERM) return 2;
 struct sock_filter filter[] = {
  BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, nr)),
  BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, 439, 0, 1),
  BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | (unsigned)error),
  BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW),
 };
 struct sock_fprog program = {sizeof(filter)/sizeof(filter[0]), filter};
 if (prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) || prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER, &program)) {
  perror("install syscall fixture"); return 1;
 }
 execvp(argv[2], argv + 2); perror("exec syscall fixture"); return 1;
}`
	path, filter := filepath.Join(dir, "filter.c"), filepath.Join(dir, "filter")
	if err := os.WriteFile(path, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(cc, "-O2", "-Wall", "-Wextra", "-Werror", path, "-o", filter).CombinedOutput(); err != nil {
		t.Fatalf("compile syscall fixture: %v\n%s", err, out)
	}
	t.Run("host_without_faccessat2", func(t *testing.T) {
		out, err := runGuestPathGate([]string{proot}, binary, source, guest, []string{filter, strconv.Itoa(int(syscall.ENOSYS))})
		if err != nil || !strings.Contains(string(out), "faccessat2=unavailable") {
			t.Fatalf("old-kernel realpath gate: %v\n%s", err, out)
		}
	})
	t.Run("host_permission_error_is_failure", func(t *testing.T) {
		out, err := runGuestPathGate([]string{proot}, binary, source, guest, []string{filter, strconv.Itoa(int(syscall.EPERM))})
		if err == nil || !strings.Contains(err.Error(), "probe host faccessat2") {
			t.Fatalf("host denial was ignored: %v\n%s", err, out)
		}
	})
	t.Run("guest_enosys_is_failure_on_capable_host", func(t *testing.T) {
		if out, err := exec.Command(binary, "--probe-faccessat2").CombinedOutput(); err != nil {
			if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 77 {
				t.Skip("host lacks faccessat2; native gate still checks realpath")
			}
			t.Fatalf("probe host: %v\n%s", err, out)
		}
		out, err := runGuestPathGate([]string{filter, strconv.Itoa(int(syscall.ENOSYS)), proot}, binary, source, guest, nil)
		if err == nil || !strings.Contains(string(out), "guest faccessat2") {
			t.Fatalf("guest syscall regression was ignored: %v\n%s", err, out)
		}
	})
}
