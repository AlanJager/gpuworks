package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/hostidentity"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

// dropInRunner is a selfinstall.Runner that keeps the files tee writes and
// rm removes, as a host would, and records each command line.
type dropInRunner struct {
	files map[string]string
	lines []string
	fail  map[string]error
}

func newDropInRunner() *dropInRunner {
	return &dropInRunner{files: map[string]string{}, fail: map[string]error{}}
}

func (r *dropInRunner) Run(_ context.Context, c selfinstall.Command) error {
	line := strings.TrimSpace(fmt.Sprintf("%s %s", c.Path, strings.Join(c.Args, " ")))
	if c.Sudo {
		line = "sudo " + line
	}
	r.lines = append(r.lines, line)
	if err := r.fail[line]; err != nil {
		return err
	}
	switch c.Path {
	case "tee":
		data, _ := io.ReadAll(c.Stdin)
		r.files[c.Args[0]] = string(data)
	case "rm":
		delete(r.files, c.Args[len(c.Args)-1])
	}
	return nil
}

func (r *dropInRunner) Output(ctx context.Context, c selfinstall.Command) (string, error) {
	return "", r.Run(ctx, c)
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

// The hook writes this drop-in, root-only and escalated through the runner,
// for a service account; for root it names no group.
func TestHostIdentityDropInGolden(t *testing.T) {
	linuxHook(t)
	layout := testLayout(t)
	run := newDropInRunner()
	if err := (hostIdentityHook{}).Install(context.Background(), run, layout, "gpuworks"); err != nil {
		t.Fatal(err)
	}
	path := "/etc/systemd/system/gpuworks.service.d/host-identity.conf"
	want := "# Written by gpuworks install: reads the host identity facts only root may read.\n" +
		"[Service]\n" +
		"ExecStartPre=-+/opt/gpuworks/gpuworks host-identity --write /run/gpuworks/host-identity.json --group gpuworks\n"
	if got := run.files[path]; got != want {
		t.Fatalf("drop-in =\n%s\nwant\n%s", got, want)
	}
	wantLines := []string{"sudo mkdir -p /etc/systemd/system/gpuworks.service.d", "sudo tee " + path, "sudo chmod 0644 " + path}
	if strings.Join(run.lines, "\n") != strings.Join(wantLines, "\n") {
		t.Fatalf("commands = %q", run.lines)
	}
	if err := (hostIdentityHook{}).Install(context.Background(), run, layout, "root"); err != nil {
		t.Fatal(err)
	}
	if got := run.files[path]; strings.Contains(got, "--group") || !strings.Contains(got, "--write "+hostidentity.RunFile+"\n") {
		t.Fatalf("root drop-in = %s", got)
	}
}

// Install twice is one drop-in; uninstall removes it, and again is no error.
func TestHostIdentityHookIsIdempotent(t *testing.T) {
	linuxHook(t)
	layout := testLayout(t)
	run := newDropInRunner()
	ctx := context.Background()
	for range 2 {
		if err := (hostIdentityHook{}).Install(ctx, run, layout, "gpuworks"); err != nil {
			t.Fatal(err)
		}
	}
	if len(run.files) != 1 {
		t.Fatalf("files = %v", run.files)
	}
	for range 2 {
		if err := (hostIdentityHook{}).Uninstall(ctx, run, layout); err != nil {
			t.Fatal(err)
		}
	}
	if len(run.files) != 0 {
		t.Fatalf("files after uninstall = %v", run.files)
	}
	// A runner failure is the hook's failure.
	run.fail["sudo rm -f "+layout.DropInPath(hostIdentityDropIn)] = errors.New("read-only file system")
	if err := (hostIdentityHook{}).Uninstall(ctx, run, layout); err == nil || !strings.Contains(err.Error(), "read-only") {
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
	hooks := Options{InstallHooks: []selfinstall.InstallHook{nopHook{}}}.installHooks()
	if len(hooks) != 2 {
		t.Fatalf("hooks = %#v", hooks)
	}
	if _, ok := hooks[0].(hostIdentityHook); !ok {
		t.Fatalf("first hook = %T", hooks[0])
	}
	if _, ok := (Options{}).installHooks()[0].(hostIdentityHook); !ok {
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
