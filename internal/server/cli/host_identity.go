package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/hostidentity"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

// hostIdentityCommand is the subcommand systemd runs as root before the
// server starts.
const hostIdentityCommand = "host-identity"

// hostIdentityDropIn names the systemd drop-in the core install hook writes.
const hostIdentityDropIn = "host-identity"

// runHostIdentity is `gpuworks host-identity --write PATH [--group GROUP]`:
// it reads the host facts only root may read and writes them to PATH,
// readable by root and GROUP (hostidentity.Write).
func runHostIdentity(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet(hostIdentityCommand, flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("write", "", "absolute path of the file to write (normally "+hostidentity.RunFile+")")
	group := fs.String("group", "", "group that may read the file (the service group); empty is root's")
	install := fs.String("install-helper", "", "internal: publish the protected systemd helper in this unit directory")
	remove := fs.String("remove-helper", "", "internal: remove this installation's protected systemd helper")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 || (*install != "" && *remove != "") || (*path != "" && (*install != "" || *remove != "")) {
		return errors.New("usage: gpuworks " + hostIdentityCommand + " --write PATH [--group GROUP]")
	}
	if *install != "" || *remove != "" {
		return manageHostIdentityHelper(*install, *remove, *group)
	}
	if *path == "" {
		return errors.New("usage: gpuworks " + hostIdentityCommand + " --write PATH [--group GROUP]")
	}
	return hostidentity.Write(*path, *group)
}

// hostIdentityHook is the core install hook: a drop-in for the server's
// systemd unit that runs `host-identity --write` as root before every
// start, using a root-protected copy of this installer, never the service's
// writable main binary. Console upgrades leave that copy in place; its v1
// facts format must remain readable until the next privileged CLI upgrade.
// "-" lets the server start when the optional identity read fails.
// The facts file is readable by the service group the unit runs
// with. Without a systemd unit directory (selfinstall clears it unless
// systemd supervises the service), or off Linux, it does nothing.
// Uninstall removes only our drop-in and protected executable.
type hostIdentityHook struct{}

var _ selfinstall.InstallHook = hostIdentityHook{}

// hostIdentityHookApplies is whether the hook acts for layout on this
// system.
func hostIdentityHookApplies(layout selfinstall.Layout) bool {
	return layout.UnitDir != "" && hostIdentityGOOS == "linux"
}

// hostIdentityGOOS is runtime.GOOS; tests set it.
var hostIdentityGOOS = runtime.GOOS

func (hostIdentityHook) Install(ctx context.Context, run selfinstall.Runner, layout selfinstall.Layout, serviceUser string) error {
	if !hostIdentityHookApplies(layout) {
		return nil
	}
	return run.Run(ctx, selfinstall.Command{Path: liveHostIdentityInstaller(),
		Args: []string{hostIdentityCommand, "--install-helper", layout.UnitDir, "--group", serviceGroupOf(serviceUser)}, Sudo: true})
}

func (hostIdentityHook) Uninstall(ctx context.Context, run selfinstall.Runner, layout selfinstall.Layout) error {
	if !hostIdentityHookApplies(layout) {
		return nil
	}
	return run.Run(ctx, selfinstall.Command{Path: liveHostIdentityInstaller(),
		Args: []string{hostIdentityCommand, "--remove-helper", layout.UnitDir}, Sudo: true})
}

// The synchronous child executes the live installer's inode even if the
// service has already replaced its pathname. The child copies /proc/self/exe.
func liveHostIdentityInstaller() string { return fmt.Sprintf("/proc/%d/exe", os.Getpid()) }

// serviceGroupOf is the group the unit runs a service account under
// (selfinstall.DefaultCredentialGroup); root and the detached mode need
// none.
func serviceGroupOf(serviceUser string) string {
	if serviceUser == "" || serviceUser == "root" {
		return ""
	}
	return selfinstall.DefaultCredentialGroup
}

// hostIdentityDropInContent is the drop-in for binary and the service group.
// The command line is quoted as the unit's own ExecStart is
// (selfinstall.SystemdCommandLine), so an install directory with a space or
// "%" works; a line break, or a "$" systemd would expand, is refused.
func hostIdentityDropInContent(binary, group string) (string, error) {
	for _, s := range []string{binary, group} {
		if strings.ContainsAny(s, "\r\n$") {
			return "", fmt.Errorf("host identity drop-in: %q holds characters a unit file would reinterpret", s)
		}
	}
	argv := []string{binary, hostIdentityCommand, "--write", hostidentity.RunFile}
	if group != "" {
		argv = append(argv, "--group", group)
	}
	line := "ExecStartPre=-+" + selfinstall.SystemdCommandLine(argv)
	return "# Written by gpuworks install: reads the host identity facts only root may read.\n[Service]\n" + line + "\n", nil
}
