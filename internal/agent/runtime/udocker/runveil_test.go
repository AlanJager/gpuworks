package udocker

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// runveilDriver is a provisioned driver in runveil mode whose udocker CLI is
// deliberately unconfigured: any CLI call fails with "entry point is not
// configured", so a passing test proves the path never touched udocker.
func runveilDriver(t *testing.T) *Driver {
	t.Helper()
	t.Setenv(launcherEnv, "runveil")
	base := t.TempDir()
	d := New(base)
	d.provisioned = true
	d.embeddedBundle = false // the udocker CLI must stay unavailable
	d.bundleDir = ""
	d.prootPath = filepath.Join(base, "udocker", "bin", "proot-x86_64")
	for _, dir := range []string{d.stateDir, filepath.Join(d.udockerDir, "containers")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// runveilContainer materializes a minimal container the way native create
// leaves it: ROOT plus container.json.
func runveilContainer(t *testing.T, d *Driver, name string) string {
	t.Helper()
	dir := d.containerDir(name)
	for _, sub := range []string{"ROOT/bin", "ROOT/etc", "ROOT/root"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "ROOT/bin/sh"), []byte("#!"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ROOT/etc/passwd"), []byte("root:x:0:0:root:/root:/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "container.json"), []byte(`{"config":{"Cmd":["/bin/sh"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunveilIsTheDefaultLauncher(t *testing.T) {
	for _, value := range []string{"", "runveil", "anything-else"} {
		t.Setenv(launcherEnv, value)
		if d := New(t.TempDir()); !d.runveil || !strings.Contains(d.Diagnostics(), "launcher: runveil") {
			t.Fatalf("%q: runveil not selected: %s", value, d.Diagnostics())
		}
	}
	t.Setenv(launcherEnv, " UDocker ")
	if d := New(t.TempDir()); d.runveil || !strings.Contains(d.Diagnostics(), "launcher: udocker") {
		t.Fatalf("udocker launcher not selectable: %s", d.Diagnostics())
	}
}

func TestRunveilPrepareUsesNoUdockerCLI(t *testing.T) {
	d := runveilDriver(t)
	dir := runveilContainer(t, d, "demo")
	// Hygon is a domestic spec: no NVIDIA setup, so the test does not depend
	// on the host's driver libraries.
	spec := runtime.WorkloadSpec{Name: "demo", Image: "example/image:tag", Env: map[string]string{"HYGON_VISIBLE_DEVICES": "0"}}
	var phases []string
	if err := d.prepareWithProgress(context.Background(), spec, func(phase, _ string) { phases = append(phases, phase) }); err != nil {
		t.Fatal(err)
	}
	if hosts, err := os.ReadFile(filepath.Join(dir, "ROOT/etc/hosts")); err != nil || !strings.Contains(string(hosts), "127.0.0.1 localhost") {
		t.Fatalf("rootfs fixups not applied: %q %v", hosts, err)
	}
	if slices.Contains(phases, "container_exec_setup") {
		t.Fatalf("runveil must not run udocker's execmode setup: %v", phases)
	}
	if _, err := os.Stat(filepath.Join(dir, "execmode")); !os.IsNotExist(err) {
		t.Fatalf("udocker execmode bookkeeping written: %v", err)
	}
}

func TestRunveilPTYArgvLaunchesProotWithExactEnvironment(t *testing.T) {
	t.Setenv("TERM", "dumb")
	d := runveilDriver(t)
	runveilContainer(t, d, "demo")
	models := t.TempDir()
	if err := d.rememberSpec("demo", runtime.WorkloadSpec{
		Name:   "demo",
		Env:    map[string]string{"VLLM_HOST_IP": "127.0.0.1", "HYGON_VISIBLE_DEVICES": "0"},
		Mounts: []runtime.Mount{{Source: models, Target: "/model"}},
	}); err != nil {
		t.Fatal(err)
	}
	argv, err := d.ExecPTY(context.Background(), "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if argv[0] != "env" || argv[1] != "-i" {
		t.Fatalf("non-root PTY must start with env -i: %q", argv)
	}
	proot := slices.Index(argv, d.prootPath)
	if proot < 0 {
		t.Fatalf("proot not invoked directly: %q", argv)
	}
	env, launch := argv[2:proot], argv[proot:]
	for _, want := range []string{"TERM=xterm-256color", "VLLM_HOST_IP=127.0.0.1", "HOME=/root", "container_execmode=P1"} {
		if !slices.Contains(env, want) {
			t.Errorf("PTY env lacks %q: %q", want, env)
		}
	}
	joined := strings.Join(launch, " ")
	for _, want := range []string{"-b " + models + ":/model", "-0", "-w /", "/bin/sh -l"} {
		if !strings.Contains(joined, want) {
			t.Errorf("PTY launch lacks %q: %s", want, joined)
		}
	}
	// Nothing but environment assignments may sit between env -i and proot:
	// no runuser/bash/python/udocker layer.
	terms := 0
	for _, entry := range env {
		if !strings.Contains(entry, "=") {
			t.Errorf("unexpected program %q between env -i and proot", entry)
		}
		if strings.HasPrefix(entry, "TERM=") {
			terms++
		}
	}
	if terms != 1 {
		t.Errorf("TERM assigned %d times; the agent's own TERM must not leak: %q", terms, env)
	}

	custom, err := d.ExecPTY(context.Background(), "demo", []string{"/bin/sh", "-c", "nvidia-smi"})
	if err != nil {
		t.Fatal(err)
	}
	if tail := custom[len(custom)-3:]; !slices.Equal(tail, []string{"/bin/sh", "-c", "nvidia-smi"}) {
		t.Fatalf("custom PTY command %q", tail)
	}
}

func TestRunveilPTYArgvSwitchesToRunUser(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Skip("no current user entry: ", err)
	}
	d := runveilDriver(t)
	runveilContainer(t, d, "demo")
	d.runUser = current.Username
	d.runUID, _ = strconv.Atoi(current.Uid)
	d.runGID, _ = strconv.Atoi(current.Gid)
	if err := d.rememberSpec("demo", runtime.WorkloadSpec{Name: "demo", Env: map[string]string{"HYGON_VISIBLE_DEVICES": "0"}}); err != nil {
		t.Fatal(err)
	}
	argv, err := d.ExecPTY(context.Background(), "demo", nil)
	if err != nil {
		t.Fatal(err)
	}
	primary, err := user.LookupGroupId(current.Gid)
	if err != nil {
		t.Skip("no primary group entry: ", err)
	}
	if !slices.Equal(argv[:5], []string{"runuser", "-u", current.Username, "-g", primary.Name}) {
		t.Fatalf("run-user PTY prefix %q", argv[:5])
	}
	sep := slices.Index(argv, "--")
	if sep < 0 || argv[sep+1] != "env" || argv[sep+2] != "-i" {
		t.Fatalf("run-user PTY must hand an exact env to proot: %q", argv)
	}
}

func TestRunveilRemoveDeletesContainerAndStateWithoutCLI(t *testing.T) {
	d := runveilDriver(t)
	dir := runveilContainer(t, d, "demo")
	// A root-owned-style fixup file is what made `udocker rm` fail; runveil
	// removes the tree directly.
	if err := os.WriteFile(filepath.Join(dir, "ROOT/etc/hosts"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".log", ".spec.json"} {
		if err := os.WriteFile(filepath.Join(d.stateDir, "demo"+suffix), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Remove(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("container dir survived: %v", err)
	}
	for _, suffix := range []string{".log", ".spec.json"} {
		if _, err := os.Stat(filepath.Join(d.stateDir, "demo"+suffix)); !os.IsNotExist(err) {
			t.Fatalf("state %s survived", suffix)
		}
	}
	if err := d.Remove(context.Background(), "demo"); err != nil {
		t.Fatalf("second remove must be idempotent: %v", err)
	}
}

func TestRemoveContainerDirNeverWidensThroughSymlinks(t *testing.T) {
	d := runveilDriver(t)
	containers := filepath.Join(d.udockerDir, "containers")

	// udocker --name alias pointing at a real container directory.
	real := runveilContainer(t, d, "3f2a-uuid")
	if err := os.Symlink(real, filepath.Join(containers, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := d.removeContainerDir("alias"); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{real, filepath.Join(containers, "alias")} {
		if _, err := os.Lstat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s survived alias removal: %v", gone, err)
		}
	}

	// A link to anywhere else only loses the link.
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep")
	if err := os.WriteFile(keep, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(containers, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := d.removeContainerDir("escape"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("removal followed a link outside the containers dir: %v", err)
	}

	for _, bad := range []string{"", ".", "..", "a/b", "../x"} {
		if err := d.removeContainerDir(bad); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
}

func TestRunveilExecValidatesBeforeLaunch(t *testing.T) {
	d := runveilDriver(t)
	if _, err := d.Exec(context.Background(), "demo", nil); err == nil || !strings.Contains(err.Error(), "requires a command") {
		t.Fatalf("empty exec: %v", err)
	}
	if _, err := d.Exec(context.Background(), "missing", []string{"/bin/sh"}); err == nil || !strings.Contains(err.Error(), "no root filesystem") {
		t.Fatalf("exec into a missing container: %v", err)
	}
	// The proot path does not exist, so the launch itself must fail loudly.
	runveilContainer(t, d, "demo")
	if _, err := d.Exec(context.Background(), "demo", []string{"/bin/sh"}); err == nil {
		t.Fatal("exec with a missing proot reported success")
	}
}
