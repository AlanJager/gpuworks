package selfinstall

import (
	"context"
	"errors"
	"os/user"
	"strings"
	"testing"
)

func TestSystemdBindsAnExplicitCredentialGroup(t *testing.T) {
	h := newHarness(t, t.TempDir())
	for _, name := range []string{DefaultServiceUser, "custom-account"} {
		cfg := h.installer.unitConfig(name, ModeSystemd)
		if cfg.ServiceGroup != DefaultCredentialGroup || !strings.Contains(RenderUnit(cfg), "\nGroup="+DefaultCredentialGroup+"\n") {
			t.Fatalf("service %s does not bind the credential group: %s", name, RenderUnit(cfg))
		}
	}
	for _, mode := range []string{ModeNohup, ModeLaunchd} {
		if cfg := h.installer.unitConfig("custom-account", mode); cfg.ServiceGroup != "" {
			t.Fatalf("%s must not authorize an ordinary primary group: %+v", mode, cfg)
		}
	}
}

func TestCredentialGroupCreationAndBusyBoxFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "groupadd", true: "addgroup fallback"}[fallback], func(t *testing.T) {
			h := newHarness(t, t.TempDir())
			exists := false
			h.installer.deps.LookupGroup = func(name string) (*user.Group, error) {
				if !exists {
					return nil, user.UnknownGroupError(name)
				}
				return credentialGroupFixture(name)
			}
			if fallback {
				h.run.fail["sudo groupadd"] = errors.New("groupadd unavailable")
			}
			h.run.on = func(cmd Command) {
				if (cmd.Path == "groupadd" || cmd.Path == "addgroup") && h.run.failure(cmd) == nil {
					exists = true
				}
			}
			if err := h.installer.ensureCredentialGroup(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !exists || !h.run.ran("sudo groupadd --system "+DefaultCredentialGroup) {
				t.Fatalf("group creation missing: %v", h.run.executed())
			}
			if fallback && !h.run.ran("sudo addgroup -S "+DefaultCredentialGroup) {
				t.Fatalf("BusyBox fallback missing: %v", h.run.executed())
			}
		})
	}
}

func TestCredentialGroupLookupFailureDoesNotChangeMembership(t *testing.T) {
	h := newHarness(t, t.TempDir())
	want := errors.New("group database unreadable")
	h.installer.deps.LookupGroup = func(string) (*user.Group, error) { return nil, want }
	if err := h.installer.ensureCredentialGroup(context.Background()); !errors.Is(err, want) {
		t.Fatalf("lookup error=%v, want %v", err, want)
	}
	if h.run.ran("sudo groupadd") || h.run.ran("sudo addgroup") {
		t.Fatalf("created a group despite an unknown lookup failure: %v", h.run.executed())
	}
}

func TestLaunchdPermissionsNeverOpenStaffGroupAccess(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.launchd = true
	h.systemd = false
	if err := h.installer.chownTree(context.Background(), "mac-operator"); err != nil {
		t.Fatal(err)
	}
	for _, command := range h.run.executed() {
		if strings.Contains(command, "g+rwX") || strings.Contains(command, "g+s") || strings.Contains(command, ":staff") {
			t.Fatalf("launchd granted the ordinary staff group access: %s", command)
		}
	}
	if !h.run.ran("sudo chmod -R go-w") {
		t.Fatalf("launchd left shared-directory replacement permissions: %v", h.run.executed())
	}
}
