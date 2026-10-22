package main

import (
	"bytes"
	"errors"
	"net"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// scriptedIO returns an IO that reads the given lines from an interactive
// terminal, so the prompts can be driven exactly the way an operator types.
func scriptedIO(t *testing.T, lines ...string) (*selfinstall.IO, *bytes.Buffer) {
	t.Helper()
	out := &bytes.Buffer{}
	io := &selfinstall.IO{
		In:         strings.NewReader(strings.Join(lines, "\n")),
		Out:        out,
		Err:        out,
		IsTerminal: func() bool { return true },
	}
	return io, out
}

func detected(ip string) func() (net.IP, error) {
	return func() (net.IP, error) { return net.ParseIP(ip), nil }
}

// Pressing enter through every question keeps the defaults: the address the
// default route leaves from, on port 8800.
func TestAskInstallPromptsEnterKeepsTheDefaults(t *testing.T) {
	io, out := scriptedIO(t, "", "", "")
	dir, addr, noSystemd := selfinstall.DefaultDir, "", false
	addrSet := false

	if err := askInstallPrompts(io, installPrompts{
		dir: &dir, addr: &addr, addrSet: &addrSet, noSystemd: &noSystemd, detect: detected("172.25.17.156"),
	}); err != nil {
		t.Fatalf("askInstallPrompts: %v", err)
	}
	if dir != selfinstall.DefaultDir || addr != "172.25.17.156:8800" || noSystemd {
		t.Fatalf("answers = (%q, %q, %v), want the defaults", dir, addr, noSystemd)
	}
	if !addrSet {
		t.Fatal("an answered question must count as an explicit choice")
	}
	if !strings.Contains(out.String(), "listen address (default: 172.25.17.156:8800)") {
		t.Fatalf("the question does not offer the detected address:\n%s", out.String())
	}
}

func TestAskInstallPromptsTakesTypedAnswers(t *testing.T) {
	io, _ := scriptedIO(t, "/home/zstack/gw", "10.0.0.7:9000", "no")
	dir, addr, noSystemd := selfinstall.DefaultDir, "", false
	addrSet := false

	if err := askInstallPrompts(io, installPrompts{
		dir: &dir, addr: &addr, addrSet: &addrSet, noSystemd: &noSystemd, detect: detected("172.25.17.156"),
	}); err != nil {
		t.Fatalf("askInstallPrompts: %v", err)
	}
	if dir != "/home/zstack/gw" || addr != "10.0.0.7:9000" || !noSystemd {
		t.Fatalf("answers = (%q, %q, %v)", dir, addr, noSystemd)
	}
}

// On a repair the installation's server.yaml is the default, so pressing enter
// keeps it instead of silently moving the server.
func TestAskInstallPromptsDefaultsToTheInstallation(t *testing.T) {
	prev := selfinstall.Existing{Config: serverconfig.File{Listen: serverconfig.Listen{Console: []string{"10.0.0.5:9999"}}}, Mode: selfinstall.ModeNohup}
	io, _ := scriptedIO(t, "", "", "")
	dir, addr, noSystemd := selfinstall.DefaultDir, "", false
	addrSet := false
	if err := askInstallPrompts(io, installPrompts{
		dir: &dir, addr: &addr, addrSet: &addrSet, noSystemd: &noSystemd,
		hasPrev: true, prev: prev, detect: detected("172.25.17.156"),
	}); err != nil {
		t.Fatalf("askInstallPrompts: %v", err)
	}
	if addr != "10.0.0.5:9999" || !noSystemd {
		t.Fatalf("answers = (%q, %v), want the installation kept", addr, noSystemd)
	}
	// A bare port keeps the address and replaces the port.
	io2, _ := scriptedIO(t, "", "7777", "")
	addr2, addrSet2, noSystemd2, dir2 := "", false, false, selfinstall.DefaultDir
	if err := askInstallPrompts(io2, installPrompts{
		dir: &dir2, addr: &addr2, addrSet: &addrSet2, noSystemd: &noSystemd2,
		hasPrev: true, prev: prev, detect: detected("172.25.17.156"),
	}); err != nil {
		t.Fatalf("askInstallPrompts: %v", err)
	}
	if addr2 != "10.0.0.5:7777" {
		t.Fatalf("addr = %q, want the host kept and the port replaced", addr2)
	}
}

// A setting named on the command line must not be asked about again.
func TestAskInstallPromptsSkipsFlagProvidedSettings(t *testing.T) {
	io, out := scriptedIO(t, "yes")
	dir, addr, noSystemd := "/custom", "10.0.0.1:9000", false
	addrSet := true

	if err := askInstallPrompts(io, installPrompts{
		dir: &dir, addr: &addr, addrSet: &addrSet, noSystemd: &noSystemd,
		skipDir: true, skipAddr: true, skipAutostart: true,
	}); err != nil {
		t.Fatalf("askInstallPrompts: %v", err)
	}
	if out.String() != "" {
		t.Fatalf("no question may be asked when every setting was flagged:\n%s", out.String())
	}
	if dir != "/custom" || addr != "10.0.0.1:9000" || noSystemd {
		t.Fatalf("answers = (%q, %q, %v)", dir, addr, noSystemd)
	}
}

// A bad answer is asked again, not carried into the install.
func TestAskInstallPromptsRejectsABadAddress(t *testing.T) {
	io, out := scriptedIO(t, "", "example.com:80", "70000", "0", "")
	dir, addr, noSystemd := selfinstall.DefaultDir, "", false
	addrSet := false

	if err := askInstallPrompts(io, installPrompts{
		dir: &dir, addr: &addr, addrSet: &addrSet, noSystemd: &noSystemd, detect: detected("10.0.0.2"),
	}); err != nil {
		t.Fatalf("askInstallPrompts: %v", err)
	}
	if addr != "10.0.0.2:8800" {
		t.Fatalf("addr = %q, want the default after three rejected answers", addr)
	}
	if got := strings.Count(out.String(), "invalid port") + strings.Count(out.String(), "enter a port or an IP:port"); got != 3 {
		t.Fatalf("the operator was told %d times, want 3:\n%s", got, out.String())
	}
}

// Without a default route or any address, the question offers every address.
func TestAskInstallPromptsFallsBackToEveryAddress(t *testing.T) {
	io, _ := scriptedIO(t, "", "", "")
	dir, addr, noSystemd := selfinstall.DefaultDir, "", false
	addrSet := false
	if err := askInstallPrompts(io, installPrompts{
		dir: &dir, addr: &addr, addrSet: &addrSet, noSystemd: &noSystemd,
		detect: func() (net.IP, error) { return nil, errors.New("no route") },
	}); err != nil {
		t.Fatal(err)
	}
	if addr != "0.0.0.0:8800" {
		t.Fatalf("addr = %q", addr)
	}
}

// A non-interactive run must not block on questions.
func TestAskInstallPromptsSkipsWithoutATerminal(t *testing.T) {
	io := &selfinstall.IO{In: strings.NewReader(""), IsTerminal: func() bool { return false }}
	dir, addr, noSystemd := selfinstall.DefaultDir, "", false
	addrSet := false
	if err := askInstallPrompts(io, installPrompts{
		dir: &dir, addr: &addr, addrSet: &addrSet, noSystemd: &noSystemd,
	}); err != nil {
		t.Fatalf("askInstallPrompts: %v", err)
	}
	if addrSet {
		t.Fatal("questions that were never asked must not mark the address as explicit")
	}
}
