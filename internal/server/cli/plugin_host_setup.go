package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"runtime"
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/extpkg/compat"
	"gpuworks.ai/gpuworks/internal/server/extpkg/hostsetup"
	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// Host setup of installed packages: the udev rule files, the device group
// and the root-owned applied records of package hostsetup. `gpuworks plugin
// host-setup` applies them by hand; the install hook below applies them on
// install and upgrade and removes them on uninstall.

// hostSetupGOOS is the platform host setup runs on; tests set another.
var hostSetupGOOS = runtime.GOOS

// newHostSetupHost is the host host setup changes, its root commands run
// through run; tests replace it with one over their own directories.
var newHostSetupHost = func(run selfinstall.Runner) hostsetup.Host {
	return hostsetup.Host{Records: hostsetup.DefaultRecords(), Run: rootRunner{run}}
}

// rootRunner runs host setup's commands through an installer runner,
// escalated with sudo when the caller is not root.
type rootRunner struct{ run selfinstall.Runner }

func (r rootRunner) Run(ctx context.Context, c hostsetup.Cmd) error {
	cmd := selfinstall.Command{Path: c.Path, Args: c.Args, Sudo: true, Stdout: io.Discard}
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	return r.run.Run(ctx, cmd)
}

func hostSetupUnsupported(id string) error {
	return messages.NewProductError(messages.ErrorPluginHostSetupUnsupported,
		"host setup steps are not supported on "+hostSetupGOOS, map[string]any{"id": id})
}

// wantedHostSetup is the host setup steps of every installed package that
// verifies and is not revoked, by id: only a signed manifest decides what
// root renders.
func wantedHostSetup(h *extpkg.Host) (hostsetup.Want, error) {
	entries, err := h.List()
	if err != nil {
		return nil, err
	}
	want := hostsetup.Want{}
	for _, e := range entries {
		if v := e.Verified; v != nil && v.Revoked == nil && len(v.Manifest.HostSetup) > 0 {
			want[e.ID] = v.Manifest.HostSetup
		}
	}
	return want, nil
}

// pluginHostSetup runs `gpuworks plugin host-setup`: with --id, that
// package's steps; otherwise every installed package's; --prune also
// removes what packages no longer installed (or no longer declaring steps)
// left. It writes nothing under the data directory, so it runs beside a
// live server; the server starts a waiting plugin when it is enabled again
// or restarted.
func (o Options) pluginHostSetup(log *slog.Logger, args []string, stdout io.Writer) error {
	fs, p := pluginFlags("host-setup")
	only := fs.String("id", "", "apply only this package's steps")
	prune := fs.Bool("prune", false, "also remove the files of packages that are no longer installed")
	serviceUser := fs.String("service-user", "", "the account the server runs as (default: the owner of the data directory)")
	if rest, err := parseInterleaved(fs, args); err != nil {
		return err
	} else if len(rest) != 0 {
		return fmt.Errorf("plugin host-setup takes no arguments; pass --id ID")
	}
	if *only != "" && *prune {
		return errors.New("plugin host-setup takes --id or --prune, not both")
	}
	if hostSetupGOOS != "linux" {
		return hostSetupUnsupported(*only)
	}
	h, st, err := o.openHost(p)
	if err != nil {
		return err
	}
	defer st.Close()
	want, err := wantedHostSetup(h)
	if err != nil {
		return err
	}
	if *only != "" {
		steps, ok := want[*only]
		if !ok {
			return o.notHostSetup(h, *only)
		}
		want = hostsetup.Want{*only: steps}
	}
	if *serviceUser == "" {
		*serviceUser = dataOwner(p.data)
	}
	host := newHostSetupHost(o.installDeps(os.Stderr).Run)
	rep, err := host.Apply(cmdContext(), want, *serviceUser, *prune)
	if err != nil {
		return err
	}
	printHostSetup(stdout, want, rep, *serviceUser)
	log.Info("plugin host setup applied", "packages", len(want), "written", rep.Written, "pruned", rep.Pruned, "user_added", rep.UserAdded)
	return nil
}

// notHostSetup is why --id names nothing host setup could apply.
func (o Options) notHostSetup(h *extpkg.Host, id string) error {
	if _, err := recheck(h, id); err != nil {
		return err
	}
	return fmt.Errorf("plugin %s declares no host setup steps", id)
}

func printHostSetup(w io.Writer, want hostsetup.Want, rep hostsetup.Report, serviceUser string) {
	for _, id := range slices.Sorted(maps.Keys(want)) {
		var devices []string
		for _, s := range want[id] {
			devices = append(devices, s.VendorID+":"+s.ProductID)
		}
		what := "already applied"
		if slices.Contains(rep.Written, id) {
			what = "applied"
		}
		fmt.Fprintf(w, "%s host setup for %s (USB %s)\n", what, id, strings.Join(devices, " "))
	}
	for _, id := range rep.Pruned {
		fmt.Fprintf(w, "removed the host setup of %s\n", id)
	}
	if len(want) == 0 && len(rep.Pruned) == 0 {
		fmt.Fprintln(w, "no installed plugin declares host setup steps")
	}
	if rep.UserAdded {
		fmt.Fprintf(w, "added %s to group %s; restart the server so it runs with the group\n", serviceUser, hostsetup.Group)
	}
	if len(rep.Written) > 0 {
		fmt.Fprintln(w, "enable the plugin again, or restart the server, to start it")
	}
}

// hostSetupLeftover is what an offline uninstall of id tells the
// administrator when id's host setup files remain on this host: the
// service account cannot remove root's files.
func hostSetupLeftover(id string) string {
	if hostSetupGOOS != "linux" {
		return ""
	}
	ids, err := newHostSetupHost(nil).Installed()
	if err != nil || !slices.Contains(ids, id) {
		return ""
	}
	return fmt.Sprintf("the host setup of %s stays on this host until you run: %s", id, hostsetup.PruneCommand)
}

// hostSetupHook is the core install hook: install and upgrade apply the
// host setup of every installed package (verified under the installation's
// server.yaml) and remove what no installed package declares; uninstall
// removes all of it. The device group stays.
type hostSetupHook struct{ opt Options }

func (k hostSetupHook) Install(ctx context.Context, run selfinstall.Runner, l selfinstall.Layout, serviceUser string) error {
	want := hostsetup.Want{}
	if declaresHostSetup(l.DataDir) {
		p := &pluginPaths{data: l.DataDir}
		if cfg := serverconfig.Path(l.Dir); l.Dir != "" && fileExists(cfg) {
			p.config = cfg
		}
		h, st, err := k.opt.openHost(p)
		if err != nil {
			return fmt.Errorf("plugin host setup: %w", err)
		}
		want, err = wantedHostSetup(h)
		st.Close()
		if err != nil {
			return fmt.Errorf("plugin host setup: %w", err)
		}
	}
	_, err := newHostSetupHost(run).Apply(ctx, want, serviceUser, true)
	return err
}

func (k hostSetupHook) Uninstall(ctx context.Context, run selfinstall.Runner, _ selfinstall.Layout) error {
	_, err := newHostSetupHost(run).Apply(ctx, nil, "", true)
	return err
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// declaresHostSetup reports whether a package installed in dataDir
// declares host setup steps, read from the manifests without verifying
// them: whether to verify at all, not what to apply.
func declaresHostSetup(dataDir string) bool {
	pkgs, err := compat.Installed(layout.New(dataDir))
	if err != nil {
		return false
	}
	return slices.ContainsFunc(pkgs, func(p compat.Package) bool { return p.Manifest != nil && len(p.Manifest.HostSetup) > 0 })
}

// installHooks are the hooks install, upgrade and uninstall run on layout:
// the core host setup hook first when this host has host setup to apply or
// remove, then the build's own. A host without any keeps exactly the
// build's hooks.
func (o Options) installHooks(l selfinstall.Layout) []selfinstall.InstallHook {
	hooks := slices.Clone(o.InstallHooks)
	if hostSetupGOOS != "linux" {
		return hooks
	}
	present, _ := newHostSetupHost(nil).Installed()
	if len(present) == 0 && !declaresHostSetup(l.DataDir) {
		return hooks
	}
	return append([]selfinstall.InstallHook{hostSetupHook{opt: o}}, hooks...)
}
