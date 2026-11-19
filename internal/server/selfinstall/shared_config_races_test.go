package selfinstall

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSharedRootPrivilegedRestoreRejectsDirectorySymlinkAtPublication(t *testing.T) {
	if sharedConfigPermissionChild(t) {
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("Linux sudo publication uses GNU ln -T; Darwin fails closed before destructive purge when native restore is unavailable")
	}
	root, _ := withSharedRoot(t)
	h := newHarness(t, root)
	h.writeExistingInstall("v1.0.0")
	h.installSelf("installed")
	h.mustInstall(h.quiet())
	private := filepath.Join(root, "server-config", "server.yaml")
	alias := filepath.Join(root, "server.yaml")
	outside := t.TempDir()
	writeFile(t, h.layout.LockPath(), "", 0o664)
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	if err := os.Chmod(filepath.Dir(private), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(private), 0o755) })
	var injected bool
	runner := &sharedConfigRunner{fakeRunner: h.run, root: root, beforeExec: func(cmd Command) error {
		if cmd.Path == "sh" && len(cmd.Args) > 3 && cmd.Args[2] == "gpuworks-config-restore" {
			injected = true
			return os.Symlink(outside, private)
		}
		return nil
	}}
	h.installer.deps.Run = runner
	h.run.fail["sudo rm -rf -- "+alias] = errors.New("cannot unlink alias")
	if _, err := h.installer.Uninstall(t.Context(), UninstallRequest{Yes: true, Purge: true, Resources: emptyUninstallResources}, nil); err == nil {
		t.Fatal("missing restore conflict")
	}
	if !injected {
		t.Fatal("fixture did not reach privileged publication after absence validation")
	}
	if target, err := os.Readlink(private); err != nil || target != outside {
		t.Fatalf("restore altered new symlink: %q %v", target, err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("restore wrote through raced target into outside directory: %v %v", entries, err)
	}
}

func TestSharedRootFailedAliasPublicationRetainsConcurrentPrivateConfig(t *testing.T) {
	if sharedConfigPermissionChild(t) {
		return
	}
	root, _ := withSharedRoot(t)
	h := newHarness(t, root)
	h.writeExistingInstall("v1.0.0")
	h.installSelf("replacement")
	old := readFile(t, h.layout.ConfigPath())
	private := filepath.Join(root, "server-config", "server.yaml")
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	h.installer.deps.Run = &sharedConfigRunner{fakeRunner: h.run, root: root}
	h.run.fail["sudo sh -c"] = errors.New("publication failed")
	h.run.on = func(cmd Command) {
		if cmd.Path == "sh" && len(cmd.Args) > 3 && cmd.Args[2] == "gpuworks-config-alias" {
			writeFile(t, private, "concurrent new config", 0o664)
		}
	}
	if _, err := h.installer.Install(t.Context(), h.quiet()); err == nil {
		t.Fatal("missing publication failure")
	}
	if readFile(t, filepath.Join(root, "server.yaml")) != old {
		t.Fatal("legacy config changed")
	}
	if readFile(t, private) != "concurrent new config" {
		t.Fatal("cleanup removed concurrent private configuration")
	}
}
