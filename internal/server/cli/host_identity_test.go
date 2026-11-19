package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

// dropInRunner records only the two exact privileged maintenance operations.
// The Linux filesystem tests exercise their actual publication behavior.
type dropInRunner struct {
	lines []string
	fail  map[string]error
}

func newDropInRunner() *dropInRunner { return &dropInRunner{fail: map[string]error{}} }

func (r *dropInRunner) Run(_ context.Context, c selfinstall.Command) error {
	line := strings.TrimSpace(fmt.Sprintf("%s %s", c.Path, strings.Join(c.Args, " ")))
	if c.Sudo {
		line = "sudo " + line
	}
	r.lines = append(r.lines, line)
	if err := r.fail[line]; err != nil {
		return err
	}
	if c.Path != liveHostIdentityInstaller() || !c.Sudo || len(c.Args) < 3 || c.Args[0] != "host-identity" {
		return fmt.Errorf("unexpected command: %s", line)
	}
	switch c.Args[1] {
	case "--install-helper":
		if len(c.Args) == 5 && c.Args[3] == "--group" {
			return nil
		}
	case "--remove-helper":
		if len(c.Args) == 3 {
			return nil
		}
	}
	return fmt.Errorf("unexpected maintenance arguments: %s", line)
}
func (r *dropInRunner) Output(context.Context, selfinstall.Command) (string, error) {
	return "", errors.New("no output command is part of the identity hook")
}

func linuxHook(t *testing.T) {
	t.Helper()
	saved := hostIdentityGOOS
	hostIdentityGOOS = "linux"
	t.Cleanup(func() { hostIdentityGOOS = saved })
}

func testLayout(t *testing.T) selfinstall.Layout {
	t.Helper()
	l := selfinstall.NewLayout("/opt/gpuworks", "", "")
	l.UnitDir = "/etc/systemd/system"
	return l
}

// Privileged maintenance always executes the live installer inode. The
// service's writable main binary is never selected as root executable.
func TestHostIdentityDropInGolden(t *testing.T) {
	linuxHook(t)
	layout := testLayout(t)
	run := newDropInRunner()
	if err := (hostIdentityHook{}).Install(context.Background(), run, layout, "gpuworks"); err != nil {
		t.Fatal(err)
	}
	want := "sudo " + liveHostIdentityInstaller() + " host-identity --install-helper /etc/systemd/system --group gpuworks"
	if len(run.lines) != 1 || run.lines[0] != want {
		t.Fatalf("commands = %q, want %s", run.lines, want)
	}
	if strings.Contains(run.lines[0], layout.Binary()) {
		t.Fatal("root runs the service-writable executable")
	}
}

func TestHostIdentityHookPropagatesMaintenanceFailure(t *testing.T) {
	linuxHook(t)
	layout := testLayout(t)
	run := newDropInRunner()
	line := "sudo " + liveHostIdentityInstaller() + " host-identity --remove-helper /etc/systemd/system"
	run.fail[line] = errors.New("read-only file system")
	if err := (hostIdentityHook{}).Uninstall(context.Background(), run, layout); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("failing uninstall = %v", err)
	}
}

// Without a unit directory, or off Linux, the hook runs nothing.
func TestHostIdentityHookNeedsSystemdOnLinux(t *testing.T) {
	layout := testLayout(t)
	run := newDropInRunner()
	saved := hostIdentityGOOS
	t.Cleanup(func() { hostIdentityGOOS = saved })
	hostIdentityGOOS = "darwin"
	if err := (hostIdentityHook{}).Install(context.Background(), run, layout, "gpuworks"); err != nil {
		t.Fatal(err)
	}
	hostIdentityGOOS = "linux"
	layout.UnitDir = ""
	if err := (hostIdentityHook{}).Install(context.Background(), run, layout, "gpuworks"); err != nil {
		t.Fatal(err)
	}
	if err := (hostIdentityHook{}).Uninstall(context.Background(), run, layout); err != nil {
		t.Fatal(err)
	}
	if len(run.lines) != 0 {
		t.Fatalf("commands = %q", run.lines)
	}
}

// A binary path is quoted as the unit's ExecStart quotes it; one a unit
// file would still reinterpret (a line break, a "$") is refused.
func TestHostIdentityDropInRefusesUnsafePaths(t *testing.T) {
	for _, bin := range []string{"/opt/a\nb/gpuworks", "/opt/$x/gpuworks", "/opt/a\rb/gpuworks"} {
		if _, err := hostIdentityDropInContent(bin, "gpuworks"); err == nil {
			t.Errorf("binary %q accepted", bin)
		}
	}
	for bin, want := range map[string]string{
		"/opt/my gpuworks/gpuworks": `ExecStartPre=-+"/opt/my gpuworks/gpuworks" host-identity --write /run/gpuworks/host-identity.json --group gpuworks`,
		"/opt/%h/gpuworks":          `ExecStartPre=-+"/opt/%%h/gpuworks" host-identity --write /run/gpuworks/host-identity.json --group gpuworks`,
		`/opt/q"t/gpuworks`:         `ExecStartPre=-+"/opt/q\"t/gpuworks" host-identity --write /run/gpuworks/host-identity.json --group gpuworks`,
	} {
		got, err := hostIdentityDropInContent(bin, "gpuworks")
		if err != nil || !strings.Contains(got, "\n"+want+"\n") {
			t.Errorf("binary %q: drop-in = %q, %v; want line %s", bin, got, err, want)
		}
		// The same quoting as the server's own command line in the unit.
		unit := selfinstall.RenderUnit(selfinstall.UnitConfig{Binary: bin, ConfigPath: "/c", DataDir: "/d"})
		if !strings.Contains(unit, "ExecStart="+selfinstall.SystemdCommandLine([]string{bin})+" ") {
			t.Errorf("binary %q quoted differently from the unit:\n%s", bin, unit)
		}
	}
}

// The core hook runs before the build's on install and after them on
// uninstall.
func TestCoreInstallHookComesFirst(t *testing.T) {
	hooks := Options{InstallHooks: []selfinstall.InstallHook{nopHook{}}}.installHooks(testLayout(t))
	if len(hooks) != 2 {
		t.Fatalf("hooks = %#v", hooks)
	}
	if _, ok := hooks[0].(hostIdentityHook); !ok {
		t.Fatalf("first hook = %T", hooks[0])
	}
	if _, ok := (Options{}).installHooks(testLayout(t))[0].(hostIdentityHook); !ok {
		t.Fatal("the community build lacks the core hook")
	}
}

// The subcommand is built in, takes --write and --group, and refuses a
// relative path or a missing --write before touching anything.
func TestHostIdentityCommand(t *testing.T) {
	var stderr bytes.Buffer
	if code := Run(discardLog(), []string{"host-identity"}, &stderr, Options{}); code != 1 {
		t.Fatalf("no --write exits %d", code)
	}
	if err := runHostIdentity([]string{"--write", "run/x.json"}, io.Discard); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative path = %v", err)
	}
	if err := runHostIdentity([]string{"--write", "/run/x.json", "extra"}, io.Discard); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("extra argument = %v", err)
	}
	if err := runHostIdentity([]string{"--bogus"}, io.Discard); err == nil {
		t.Fatal("an unknown flag was accepted")
	}
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "shadows a built-in") {
			t.Fatalf("recover = %v", r)
		}
	}()
	Run(discardLog(), nil, io.Discard, Options{Commands: []Command{{Name: "host-identity", Run: func(context.Context, Env, []string) error { return nil }}}})
}
