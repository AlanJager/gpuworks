package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/extpkg/hostsetup"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

// TestMain points host setup at empty directories of its own, so no test
// reads this host's udev rules or applied records: a host where `plugin
// host-setup` once ran would otherwise change the hooks every install,
// upgrade and uninstall test runs. A test that applies host setup sets its
// own (fakeHostSetup); one that reaches root commands without it fails.
func TestMain(m *testing.M) {
	serveStopTestChild()
	root, err := os.MkdirTemp("", "gpuworks-cli-hostsetup-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	newHostSetupHost = func(selfinstall.Runner) hostsetup.Host {
		return hostsetup.Host{
			RulesDir: filepath.Join(root, "rules.d"),
			Records:  hostsetup.Records{Dir: filepath.Join(root, "plugins"), Owner: os.Getuid()},
			Run:      refuseRoot{},
		}
	}
	code := m.Run()
	os.RemoveAll(root)
	os.Exit(code)
}

type refuseRoot struct{}

func (refuseRoot) Run(context.Context, hostsetup.Cmd) error {
	return errors.New("this test did not set up host setup (fakeHostSetup)")
}
