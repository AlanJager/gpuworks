package selfinstall

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// The test account stands in for the unprivileged service. A 0555 shared
// parent denies it the same atomic-write capability as root's 0755 directory.
// Only the privileged command fixture temporarily obtains write access;
// application config writes never receive that capability.
type sharedConfigRunner struct {
	*fakeRunner
	root       string
	beforeExec func(Command) error
}

func (r *sharedConfigRunner) privileged(ctx context.Context, cmd Command) (string, error) {
	info, err := os.Stat(r.root)
	if err != nil {
		return "", err
	}
	if err := os.Chmod(r.root, 0o755); err != nil {
		return "", err
	}
	defer func() { _ = os.Chmod(r.root, info.Mode().Perm()) }()
	private := filepath.Join(r.root, "server-config")
	if privateInfo, err := os.Stat(private); err == nil && privateInfo.IsDir() {
		if err := os.Chmod(private, 0o755); err != nil {
			return "", err
		}
		defer func() { _ = os.Chmod(private, privateInfo.Mode()) }()
	}
	if r.beforeExec != nil {
		if err := r.beforeExec(cmd); err != nil {
			return "", err
		}
	}
	cmd.Sudo = false
	return (ExecRunner{Euid: func() int { return 0 }}).Output(ctx, cmd)
}
func (r *sharedConfigRunner) Run(ctx context.Context, cmd Command) error {
	if err := r.fakeRunner.Run(ctx, cmd); err != nil {
		return err
	}
	switch cmd.Path {
	case "mkdir", "chmod", "find", "sh", "rm", "rmdir":
		_, err := r.privileged(ctx, cmd)
		return err
	}
	return nil
}
func (r *sharedConfigRunner) Output(ctx context.Context, cmd Command) (string, error) {
	if cmd.Path == "mktemp" {
		if err := r.fakeRunner.Run(ctx, cmd); err != nil {
			return "", err
		}
		return r.privileged(ctx, cmd)
	}
	return r.fakeRunner.Output(ctx, cmd)
}

func TestSharedRootInstallRepairAllowsAtomicSettingsSave(t *testing.T) {
	root, agent := withSharedRoot(t)
	h := newHarness(t, root)
	h.writeExistingInstall("v1.0.0")
	original := readFile(t, h.layout.ConfigPath()) + "\n# operator note survives repair\nmetrics_retention: 168h\n"
	writeFile(t, h.layout.ConfigPath(), original, 0o664)
	h.installSelf("repaired binary")
	runner := &sharedConfigRunner{fakeRunner: h.run, root: root}
	h.installer.deps.Run = runner
	rootMode := os.FileMode(0o555)
	if os.Geteuid() == 0 {
		rootMode = 0o755
	}
	if err := os.Chmod(root, rootMode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	if err := saveConfigAsService(t, root, h.layout.ConfigPath()); err == nil {
		t.Fatal("permission fixture allowed atomic writes to the old shared parent")
	}
	req := h.quiet()
	req.ServiceUser = DefaultServiceUser
	report := h.mustInstall(req)
	path := report.Layout.ConfigPath()
	if err := saveConfigAsService(t, root, path); err != nil {
		t.Fatalf("service cannot atomically save settings after install repair: %v", err)
	}
	if path != filepath.Join(root, "server-config", "server.yaml") {
		t.Fatalf("config path=%q", path)
	}
	if directory := DirOfConfig(path); directory != root {
		t.Fatalf("private config belongs to installation %q, want %q", directory, root)
	}
	if target, err := os.Readlink(filepath.Join(root, "server.yaml")); err != nil || target != "server-config/server.yaml" {
		t.Fatalf("compatibility alias=%q %v", target, err)
	}
	if cfg, err := serverconfig.Load(path); err != nil || cfg.MetricsRetention != "168h" {
		t.Fatalf("retained config=%+v %v", cfg, err)
	}
	if !strings.Contains(readFile(t, path), "# operator note survives repair") {
		t.Fatal("repair dropped the operator's comment")
	}
	if info, err := os.Stat(root); err != nil || info.Mode().Perm() != rootMode {
		t.Fatalf("shared directory permissions changed: %v %v", info, err)
	}
	for _, p := range agent {
		if readFile(t, p) != "agent state" {
			t.Fatalf("agent file changed: %s", p)
		}
	}
}

// Root CI must exercise the service's DAC boundary, not root's bypass. Only
// this fixture's traversable parent and private config are shared with the
// child. No real host account, agent state or directory permission is changed.
func saveConfigAsService(t *testing.T, root, path string) error {
	t.Helper()
	if os.Geteuid() != 0 {
		return serverconfig.SetAutoCheckUpdates(path, true)
	}
	const serviceID = 65534
	parent := filepath.Dir(root)
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(parent, "service-settings-probe")
	if _, err := os.Stat(probe); os.IsNotExist(err) {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if err := copyFileAtomic(executable, probe, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if path == filepath.Join(root, "server-config", "server.yaml") {
		for _, owned := range []string{filepath.Dir(path), path} {
			if err := os.Chown(owned, serviceID, serviceID); err != nil {
				t.Fatal(err)
			}
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, probe, "-test.run=^TestSharedConfigServiceProcess$")
	command.Env = append(os.Environ(), "GPUWORKS_TEST_SHARED_CONFIG_PATH="+path)
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: serviceID, Gid: serviceID}}
	if out, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("service UID atomic save: %w: %s", err, out)
	}
	return nil
}

func TestSharedConfigServiceProcess(t *testing.T) {
	path := os.Getenv("GPUWORKS_TEST_SHARED_CONFIG_PATH")
	if path == "" {
		return
	}
	if os.Geteuid() != 65534 {
		t.Fatal("service fixture did not drop privileges")
	}
	if err := serverconfig.SetAutoCheckUpdates(path, true); err != nil {
		t.Fatal(err)
	}
}

// Execute permission-failure paths under an actual ordinary identity when the
// test runner is root. This prevents DAC bypass from hiding their sudo branch.
func sharedConfigPermissionChild(t *testing.T) bool {
	t.Helper()
	if os.Geteuid() != 0 {
		return false
	}
	dir := t.TempDir()
	if err := os.Chmod(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(dir, "service-settings-probe")
	if err := copyFileAtomic(executable, probe, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, probe, "-test.run=^"+t.Name()+"$", "-test.v")
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("unprivileged permission regression: %v\n%s", err, out)
	}
	return true
}
