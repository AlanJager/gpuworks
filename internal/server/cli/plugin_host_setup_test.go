package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/extpkg/hostsetup"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// sudoFiles stands in for the installer's runner under sudo: it carries
// out the file commands host setup issues on the test's directories, as
// coreutils would, records each command, and refuses one not escalated
// or outside the test root.
type sudoFiles struct {
	t    *testing.T
	root string
	cmds []string
}

func (s *sudoFiles) Run(_ context.Context, c selfinstall.Command) error {
	if !c.Sudo {
		s.t.Fatalf("%s is not escalated", c)
	}
	s.cmds = append(s.cmds, c.Path+" "+strings.Join(c.Args, " "))
	args := slices.DeleteFunc(slices.Clone(c.Args), func(a string) bool { return a == "--" })
	path := func(p string) string {
		if !strings.HasPrefix(p, s.root+string(filepath.Separator)) {
			s.t.Fatalf("%s touches %s outside the test root", c, p)
		}
		return p
	}
	switch c.Path {
	case "tee":
		body, err := io.ReadAll(c.Stdin)
		if err != nil {
			return err
		}
		return os.WriteFile(path(args[0]), body, 0o600)
	case "chmod":
		return os.Chmod(path(args[1]), 0o644)
	case "rm":
		if err := os.Remove(path(args[1])); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	case "mkdir":
		return os.MkdirAll(path(args[len(args)-1]), 0o755)
	case "udevadm", "groupadd", "usermod":
		return nil
	}
	s.t.Fatalf("unknown command %s", c)
	return nil
}

func (s *sudoFiles) Output(context.Context, selfinstall.Command) (string, error) {
	return "", errors.New("host setup reads no command output")
}

// fakeHostSetup points host setup at the test's directories for the rest
// of the test and returns its runner and the records.
func fakeHostSetup(t *testing.T) (*sudoFiles, string, hostsetup.Records) {
	t.Helper()
	root := t.TempDir()
	rules := filepath.Join(root, "rules.d")
	if err := os.Mkdir(rules, 0o755); err != nil {
		t.Fatal(err)
	}
	run := &sudoFiles{t: t, root: root}
	records := hostsetup.Records{Dir: filepath.Join(root, "plugins"), Owner: os.Getuid()}
	old := newHostSetupHost
	newHostSetupHost = func(selfinstall.Runner) hostsetup.Host {
		return hostsetup.Host{RulesDir: rules, Records: records, Run: rootRunner{run},
			GroupExists: func(string) (bool, error) { return true, nil },
			InGroup:     func(string, string) (bool, error) { return true, nil }}
	}
	t.Cleanup(func() { newHostSetupHost = old })
	return run, rules, records
}

var usbKey = extapi.HostSetupStep{Type: extapi.HostSetupUSBDevice, VendorID: "096e", ProductID: "0201"}

func (e *pluginEnv) installWithHostSetup(version string, steps ...extapi.HostSetupStep) {
	e.t.Helper()
	spec := extsign.Minimal("acme", "acme_backup", version)
	if len(steps) > 0 {
		spec.Manifest.Requires = map[string]int{extapi.CapHostSetup: 1}
		spec.Manifest.HostSetup = steps
	}
	path := e.writePackage("p-"+version+".gwpkg", extsign.Package(e.t, e.pub, spec))
	if _, err := e.run("install", "--file", path); err != nil {
		e.t.Fatal(err)
	}
}

// host-setup renders the installed package's steps as root (every command
// escalated), records them, and says so; run again it changes nothing.
// Uninstalling offline names the prune command, which removes both files.
func TestPluginHostSetupAppliesAndPrunes(t *testing.T) {
	run, rules, records := fakeHostSetup(t)
	e := newPluginEnv(t, testPluginSite)
	if out, err := e.run("host-setup"); err != nil || !strings.Contains(out, "no installed plugin declares host setup steps") {
		t.Fatalf("with nothing installed = %q, %v", out, err)
	}
	e.installWithHostSetup("1.0.0", usbKey)
	out, err := e.run("host-setup", "--service-user", "gpuworks")
	if err != nil || !strings.Contains(out, "applied host setup for acme_backup (USB 096e:0201)") {
		t.Fatalf("host-setup = %q, %v", out, err)
	}
	if ok, err := records.Matches("acme_backup", []extapi.HostSetupStep{usbKey}); !ok || err != nil {
		t.Fatalf("record = %v, %v", ok, err)
	}
	name, _ := hostsetup.RuleFile("acme_backup")
	body, err := os.ReadFile(filepath.Join(rules, name))
	if err != nil || !strings.Contains(string(body), `ATTR{idVendor}=="096e"`) {
		t.Fatalf("rule file = %q, %v", body, err)
	}
	run.cmds = nil
	if out, err := e.run("host-setup", "--id", "acme_backup"); err != nil || !strings.Contains(out, "already applied host setup for acme_backup") || len(run.cmds) != 0 {
		t.Fatalf("again = %q, %v, ran %q", out, err, run.cmds)
	}

	out, err = e.run("uninstall", "acme_backup")
	if err != nil || !strings.Contains(out, "stays on this host until you run: "+hostsetup.PruneCommand) {
		t.Fatalf("uninstall = %q, %v", out, err)
	}
	out, err = e.run("host-setup", "--prune")
	if err != nil || !strings.Contains(out, "removed the host setup of acme_backup") {
		t.Fatalf("prune = %q, %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(rules, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rule file after prune: %v", err)
	}
	if _, err := records.Read("acme_backup"); !errors.Is(err, hostsetup.ErrNotApplied) {
		t.Fatalf("record after prune: %v", err)
	}
}

// host-setup refuses what it cannot apply: an id that is not installed, a
// package without steps, both --id and --prune, and any platform but Linux.
func TestPluginHostSetupRefusals(t *testing.T) {
	fakeHostSetup(t)
	e := newPluginEnv(t, testPluginSite)
	if _, err := e.run("host-setup", "--id", "acme_backup"); productCode(err) != messages.ErrorPluginNotInstalled {
		t.Fatalf("not installed = %v", err)
	}
	e.installWithHostSetup("1.0.0")
	if _, err := e.run("host-setup", "--id", "acme_backup"); err == nil || !strings.Contains(err.Error(), "declares no host setup steps") {
		t.Fatalf("without steps = %v", err)
	}
	if _, err := e.run("host-setup", "--id", "acme_backup", "--prune"); err == nil {
		t.Fatal("--id with --prune accepted")
	}
	if out, _ := e.run("uninstall", "acme_backup"); strings.Contains(out, hostsetup.PruneCommand) {
		t.Fatalf("an uninstall without host setup files named the prune command:\n%s", out)
	}
	old := hostSetupGOOS
	hostSetupGOOS = "darwin"
	t.Cleanup(func() { hostSetupGOOS = old })
	if _, err := e.run("host-setup"); productCode(err) != messages.ErrorPluginHostSetupUnsupported {
		t.Fatalf("on darwin = %v", err)
	}
	if hooks := e.opt.installHooks(selfinstall.Layout{DataDir: e.data}); len(hooks) != 0 {
		t.Fatalf("darwin hooks = %v", hooks)
	}
}

// The install hook joins install, upgrade and uninstall only on a host with
// host setup to apply or remove; it applies every installed package's
// steps through the installer's runner and removes them all on uninstall.
func TestHostSetupInstallHook(t *testing.T) {
	run, _, records := fakeHostSetup(t)
	e := newPluginEnv(t, testPluginSite)
	l := selfinstall.Layout{Dir: filepath.Dir(e.config), DataDir: e.data}
	if hooks := e.opt.installHooks(l); len(hooks) != 0 {
		t.Fatalf("hooks with nothing to set up = %v", hooks)
	}
	e.installWithHostSetup("1.0.0", usbKey)
	hooks := e.opt.installHooks(l)
	if len(hooks) != 1 {
		t.Fatalf("hooks = %v", hooks)
	}
	if err := hooks[0].Install(context.Background(), nil, l, "gpuworks"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := records.Matches("acme_backup", []extapi.HostSetupStep{usbKey}); !ok || len(run.cmds) == 0 {
		t.Fatalf("install hook applied nothing: %q", run.cmds)
	}
	if err := hooks[0].Uninstall(context.Background(), nil, l); err != nil {
		t.Fatal(err)
	}
	if _, err := records.Read("acme_backup"); !errors.Is(err, hostsetup.ErrNotApplied) {
		t.Fatalf("record after the uninstall hook: %v", err)
	}
}
