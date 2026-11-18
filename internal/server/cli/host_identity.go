package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" || fs.NArg() > 0 {
		return errors.New("usage: gpuworks " + hostIdentityCommand + " --write PATH [--group GROUP]")
	}
	return hostidentity.Write(*path, *group)
}

// hostIdentityHook is the core install hook: a drop-in for the server's
// systemd unit that runs `host-identity --write` as root before every
// start ("+" gives that step full privileges whatever account the service
// runs as; "-" lets the server start when the step fails, for instance
// after a rollback to a release without the subcommand, and read what it
// can itself). The file is readable by the service group the unit runs
// with. Without a systemd unit directory, or off Linux, it does nothing.
// Uninstall removes the drop-in.
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
	content, err := hostIdentityDropInContent(layout.Binary(), serviceGroupOf(serviceUser))
	if err != nil {
		return err
	}
	return selfinstall.WriteDropIn(ctx, run, layout, hostIdentityDropIn, content)
}

func (hostIdentityHook) Uninstall(ctx context.Context, run selfinstall.Runner, layout selfinstall.Layout) error {
	if !hostIdentityHookApplies(layout) {
		return nil
	}
	return selfinstall.RemoveDropIn(ctx, run, layout, hostIdentityDropIn)
}

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
func hostIdentityDropInContent(binary, group string) (string, error) {
	for _, s := range []string{binary, group} {
		if strings.ContainsAny(s, " \t\r\n\"'\\%$;") {
			return "", fmt.Errorf("host identity drop-in: %q holds characters a unit file would reinterpret", s)
		}
	}
	line := "ExecStartPre=-+" + binary + " " + hostIdentityCommand + " --write " + hostidentity.RunFile
	if group != "" {
		line += " --group " + group
	}
	return "# Written by gpuworks install: reads the host identity facts only root may read.\n[Service]\n" + line + "\n", nil
}
