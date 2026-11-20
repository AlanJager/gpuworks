//go:build linux

package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// Run the production shell through a sudo fixture that enforces noninteractive
// invocation but retains the test account's privileges. Directory operations
// themselves are real: mkdir, id, chown, mktemp, and /bin/sh are not mocked.
func clusterDirectorySudoFixture(t *testing.T, deny bool) (string, string) {
	t.Helper()
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "sudo-calls")
	script := "#!/bin/sh\nset -eu\ntest \"$1\" = -n || exit 91\nshift\nprintf '%s\\n' \"$1\" >> " + clusterInstallQuote(log) + "\n"
	if deny {
		script += "echo 'sudo: a password is required' >&2\nexit 1\n"
	} else {
		script += "exec \"$@\"\n"
	}
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	return account.Username, log
}

func runClusterDirectoryCommand(t *testing.T, node clusterInstallNode, noService, create bool) error {
	t.Helper()
	command := "set -eu; " + clusterInstallDirectoryCommand(node, noService, create)
	raw, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", command).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(raw)), err)
	}
	return nil
}

func assertClusterDirectoryEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("directory check mutated %s: entries=%v, error=%v", path, entries, err)
	}
}

func TestClusterInstallSystemDirectoryCheckDoesNotCreateMissingParents(t *testing.T) {
	account, log := clusterDirectorySudoFixture(t, false)
	parent := t.TempDir()
	target := filepath.Join(parent, "missing", "nested", "server")
	node := clusterInstallNode{Dir: target, SSH: clusterInstallSSH{User: account}}
	if err := runClusterDirectoryCommand(t, node, false, false); err != nil {
		t.Fatal(err)
	}
	assertClusterDirectoryEmpty(t, parent)
	raw, err := os.ReadFile(log)
	if err != nil || string(raw) != "sh\n" {
		t.Fatalf("directory capability was not checked with sudo -n: %q, %v", raw, err)
	}
}

func TestClusterInstallSystemDirectoryCreatesOnlyMissingPathsAndPreservesParents(t *testing.T) {
	account, _ := clusterDirectorySudoFixture(t, false)
	parent := t.TempDir()
	if err := os.Chmod(parent, 0710); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(parent, "gpuworks")
	second := filepath.Join(first, "control")
	target := filepath.Join(second, "server")
	node := clusterInstallNode{Dir: target, SSH: clusterInstallSSH{User: account}}
	if err := runClusterDirectoryCommand(t, node, false, true); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{parent: 0710, first: 0755, second: 0755, target: 0700} {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != mode {
			t.Fatalf("wrong directory permissions at %s: info=%v, error=%v; want %04o", path, info, err, mode)
		}
		stat := info.Sys().(*syscall.Stat_t)
		if stat.Uid != uint32(os.Getuid()) || stat.Gid != uint32(os.Getgid()) {
			t.Fatalf("unexpected directory ownership at %s: %d:%d", path, stat.Uid, stat.Gid)
		}
	}
	assertClusterDirectoryEmpty(t, target)
	if err := runClusterDirectoryCommand(t, node, false, true); err == nil {
		t.Fatal("second creation adopted an existing installation")
	}
}

func TestClusterInstallDirectoryRejectsExistingOrRedirectedPaths(t *testing.T) {
	for _, kind := range []string{"target-directory", "target-file", "target-dangling-symlink", "parent-symlink", "parent-dangling-symlink", "parent-file"} {
		t.Run(kind, func(t *testing.T) {
			account, _ := clusterDirectorySudoFixture(t, false)
			root := t.TempDir()
			target := filepath.Join(root, "server")
			var err error
			switch kind {
			case "target-directory":
				err = os.Mkdir(target, 0751)
			case "target-file":
				err = os.WriteFile(target, []byte("retained contents"), 0640)
			case "target-dangling-symlink":
				err = os.Symlink(filepath.Join(root, "absent"), target)
			case "parent-symlink", "parent-dangling-symlink":
				other := filepath.Join(root, "other")
				if kind == "parent-symlink" {
					err = os.Mkdir(other, 0751)
					if err != nil {
						t.Fatal(err)
					}
				}
				link := filepath.Join(root, "parent")
				err = os.Symlink(other, link)
				target = filepath.Join(link, "server")
			case "parent-file":
				file := filepath.Join(root, "parent")
				err = os.WriteFile(file, []byte("retained contents"), 0640)
				target = filepath.Join(file, "server")
			}
			if err != nil {
				t.Fatal(err)
			}
			before := clusterDirectoryTree(t, root)
			node := clusterInstallNode{Dir: target, SSH: clusterInstallSSH{User: account}}
			for _, noService := range []bool{false, true} {
				if err := runClusterDirectoryCommand(t, node, noService, false); err == nil {
					t.Errorf("unsafe directory check succeeded (no-service=%v)", noService)
				}
			}
			if err := runClusterDirectoryCommand(t, node, false, true); err == nil {
				t.Error("unsafe privileged directory creation succeeded")
			}
			if after := clusterDirectoryTree(t, root); !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected path mutated existing objects: before=%v, after=%v", before, after)
			}
		})
	}
}

func clusterDirectoryTree(t *testing.T, root string) map[string]string {
	t.Helper()
	state := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := info.Mode().String()
		if info.Mode().IsRegular() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += ":" + string(raw)
		} else if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value += ":" + link
		}
		state[path] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestClusterInstallSystemDirectorySudoDenialLeavesNoFiles(t *testing.T) {
	account, _ := clusterDirectorySudoFixture(t, true)
	parent := t.TempDir()
	target := filepath.Join(parent, "missing", "server")
	node := clusterInstallNode{Dir: target, SSH: clusterInstallSSH{User: account}}
	for _, create := range []bool{false, true} {
		if err := runClusterDirectoryCommand(t, node, false, create); err == nil || !strings.Contains(err.Error(), "password is required") {
			t.Fatalf("sudo denial not propagated (create=%v): %v", create, err)
		}
	}
	assertClusterDirectoryEmpty(t, parent)
}

func TestClusterInstallNoServiceDirectoryRequiresExistingWritableParentWithoutSudo(t *testing.T) {
	for _, kind := range []string{"writable", "missing-parent", "unwritable-parent"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "unwritable-parent" && os.Geteuid() == 0 {
				t.Skip("root bypasses directory mode restrictions")
			}
			account, log := clusterDirectorySudoFixture(t, true)
			parent := t.TempDir()
			target := filepath.Join(parent, "server")
			if kind == "missing-parent" {
				target = filepath.Join(parent, "missing", "server")
			} else if kind == "unwritable-parent" {
				if err := os.Chmod(parent, 0500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
			}
			node := clusterInstallNode{Dir: target, SSH: clusterInstallSSH{User: account}}
			if err := runClusterDirectoryCommand(t, node, true, false); (err == nil) != (kind == "writable") {
				t.Fatalf("wrong no-service check result for %s: %v", kind, err)
			}
			assertClusterDirectoryEmpty(t, parent)
			if kind == "writable" {
				if err := runClusterDirectoryCommand(t, node, true, true); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(target)
				if err != nil || info.Mode().Perm() != 0700 {
					t.Fatalf("no-service directory is not private: info=%v, error=%v", info, err)
				}
			}
			if _, err := os.Lstat(log); !os.IsNotExist(err) {
				t.Fatalf("no-service directory operation invoked sudo: %v", err)
			}
		})
	}
}

func TestClusterInstallDirectoryDefaultsRespectServiceModeAndExplicitFlags(t *testing.T) {
	for _, test := range []struct {
		name, account, explicit, want string
		noService                     bool
	}{
		{name: "system", account: "operator", want: "/opt/gpuworks/server"},
		{name: "system-root", account: "root", want: "/opt/gpuworks/server"},
		{name: "system-explicit", account: "operator", explicit: "/srv/custom/server", want: "/srv/custom/server"},
		{name: "no-service", account: "operator", noService: true, want: "/home/operator/gpuworks-cluster-new"},
		{name: "no-service-root", account: "root", noService: true, want: "/root/gpuworks-cluster-new"},
		{name: "no-service-explicit", account: "operator", noService: true, explicit: "/srv/custom/server", want: "/srv/custom/server"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"--binary", "/tmp/gpuworks-ha", "--hosts", "192.0.2.11,192.0.2.12,192.0.2.13", "--ssh-user", test.account}
			if test.explicit != "" {
				args = append(args, "--dir", test.explicit)
			}
			if test.noService {
				args = append(args, "--no-service")
			}
			o, err := parseClusterOptions(args, io.Discard, true)
			if err != nil || o.Dir != test.want {
				t.Fatalf("wrong flag directory: got %q, want %q, error=%v", o.Dir, test.want, err)
			}
		})
	}
}

func TestClusterInstallDirectoryConfigDefaultsResolveNoServiceAccount(t *testing.T) {
	clusterWizardSSHFixture(t, false)
	config := writeClusterConfig(t, "version: 1\nnodes:\n  - address: 192.0.2.11\n  - address: 192.0.2.12\n    ssh:\n      user: root\n  - address: 192.0.2.13\n    dir: /srv/custom/server\n")
	for _, noService := range []bool{false, true} {
		args := []string{"--binary", "/tmp/gpuworks-ha", "--config", config}
		if noService {
			args = append(args, "--no-service")
		}
		o, err := parseClusterOptions(args, io.Discard, true)
		if err != nil {
			t.Fatal(err)
		}
		if noService && (o.Nodes[0].Dir != "" || o.Nodes[1].Dir != "") {
			t.Fatal("no-service default was selected before OpenSSH resolved each account")
		}
		if err := resolveClusterInstallSSH(t.Context(), &o); err != nil {
			t.Fatal(err)
		}
		want := []string{"/opt/gpuworks/server", "/opt/gpuworks/server", "/srv/custom/server"}
		if noService {
			want[0], want[1] = "/home/operator/gpuworks-cluster-new", "/root/gpuworks-cluster-new"
		}
		for i, node := range o.Nodes {
			if node.Dir != want[i] {
				t.Errorf("node %d directory = %q, want %q (no-service=%v)", i, node.Dir, want[i], noService)
			}
		}
	}
}
