package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

type nopHook struct{}

func (nopHook) Install(context.Context, selfinstall.Runner, selfinstall.Layout, string) error {
	return nil
}
func (nopHook) Uninstall(context.Context, selfinstall.Runner, selfinstall.Layout) error { return nil }

// The build's distribution profile reaches the install dependencies, so the
// upgrade command reads that edition's channel; the zero Options are the
// community channel.
func TestInstallDepsCarryTheBuildsProfile(t *testing.T) {
	if got := (Options{}).installDeps(nil).Profile; !got.IsZero() {
		t.Fatalf("community profile = %+v", got)
	}
	profile := distribution.Profile{Edition: "acme", Sources: []string{"https://acme.example"}}
	deps := Options{Distribution: profile}.installDeps(nil)
	if deps.Profile.Edition != "acme" || strings.Join(deps.Profile.Sources, ",") != "https://acme.example" || deps.Run == nil {
		t.Fatalf("deps = %+v", deps)
	}
}

func TestRunPanicsOnANilInstallHook(t *testing.T) {
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "install hook 2 is nil") {
			t.Fatalf("recover = %v", r)
		}
	}()
	Run(discardLog(), nil, &bytes.Buffer{}, Options{InstallHooks: []selfinstall.InstallHook{nopHook{}, nil}})
}

// --allow-edition-change is a flag of upgrade: it parses, and the command
// goes on to the checks that follow parsing.
func TestUpgradeTakesAllowEditionChange(t *testing.T) {
	err := upgradeCommand(context.Background(), []string{"--allow-edition-change", "--archive", " "}, Options{})
	if err == nil || !strings.Contains(err.Error(), "needs a path") {
		t.Fatalf("err = %v", err)
	}
}
