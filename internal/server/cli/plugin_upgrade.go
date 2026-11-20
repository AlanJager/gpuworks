package cli

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
)

// Upgrade and rollback with the server stopped. A running server holds the
// package in memory and upgrades it through its own API (POST
// /api/v1/plugins/{id}/upgrade and /rollback), where the new version's
// health gate and the automatic rollback run; the command line has no
// administrator session to call it with, so it refuses then and names the
// route.

// offlineOnly turns the refusal of a running server into one that names
// the API route doing the same while it runs.
func offlineOnly(err error, route string) error {
	var running *serverRunningError
	if errors.As(err, &running) {
		return fmt.Errorf("%w; while it runs, use the console or POST /api/v1/plugins/%s", err, route)
	}
	return err
}

// pluginUpgrade replaces an installed package with the package in --file.
// It prints what the new version asks for compared with the installed one
// and changes nothing without --approve (unless the approval hash did not
// change and the site policy auto-approves that). It snapshots the
// plugin's data first; the new version starts with the next server, and
// plugin rollback ID --restore-state goes back to the old one with that
// data.
func (o Options) pluginUpgrade(log *slog.Logger, args []string, stdout io.Writer) error {
	fs, p := pluginFlags("upgrade")
	file := fs.String("file", "", "the .gwpkg package to upgrade to")
	approve := fs.Bool("approve", false, "approve what the new version asks for")
	if _, err := parseInterleaved(fs, args); err != nil {
		return err
	}
	if *file == "" {
		return errors.New("plugin upgrade needs --file PKG.gwpkg")
	}
	h, _, done, err := o.openStoppedHost(p)
	if err != nil {
		return offlineOnly(err, "<id>/upgrade")
	}
	defer done()
	owners, err := installedFeatureOwners(h)
	if err != nil {
		return err
	}
	opt := extpkg.UpgradeOptions{ApprovedBy: "command line", Features: owners, Snapshot: true}
	up, err := upgradeFrom(h, *file, opt)
	var need *extpkg.ApprovalNeeded
	if errors.As(err, &need) {
		fmt.Fprintln(stdout, describeDiff(need.ID, need.Diff))
		if !*approve {
			return fmt.Errorf("nothing was changed; run plugin upgrade --file %s --approve to approve it", *file)
		}
		opt.Approve = need.Hash
		up, err = upgradeFrom(h, *file, opt)
	}
	if err != nil {
		return err
	}
	m := up.Manifest
	how := "approved"
	if up.AutoApproved {
		how = "auto-approved (approval unchanged)"
	}
	fmt.Fprintf(stdout, "upgraded %s from %s to %s, %s %s\n", m.ID, up.Diff.FromVersion, m.Version, how, up.ApprovalHash)
	fmt.Fprintf(stdout, "data of %s kept as snapshot %s; plugin rollback %s --restore-state goes back to it\n", up.Diff.FromVersion, up.Snapshot.Name, m.ID)
	log.Info("plugin upgraded", "id", m.ID, "from", up.Diff.FromVersion, "to", m.Version, "snapshot", up.Snapshot.Name, "data", p.data)
	return nil
}

func upgradeFrom(h *extpkg.Host, file string, opt extpkg.UpgradeOptions) (*extpkg.Upgraded, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return h.Upgrade(f, opt)
}

// describeDiff is what an upgrade asks for compared with the installed
// version, one change per line.
func describeDiff(id string, d extpkg.ApprovalDiff) string {
	lines := []string{fmt.Sprintf("upgrading %s from %s to %s asks for:", id, d.FromVersion, d.ToVersion)}
	add := func(what string, added, removed []string) {
		if len(added) > 0 {
			lines = append(lines, fmt.Sprintf("  + %s: %s", what, strings.Join(added, ", ")))
		}
		if len(removed) > 0 {
			lines = append(lines, fmt.Sprintf("  - %s: %s", what, strings.Join(removed, ", ")))
		}
	}
	perms := func(list []extpkg.PermissionChange) []string {
		out := make([]string, len(list))
		for i, p := range list {
			out[i] = p.Permission + " (" + p.Risk + ")"
		}
		return out
	}
	add("permissions", perms(d.PermissionsAdded), perms(d.PermissionsRemoved))
	add("roles", d.RolesAdded, d.RolesRemoved)
	add("capabilities", d.CapabilitiesAdded, d.CapabilitiesRemoved)
	add("features", d.FeaturesAdded, d.FeaturesRemoved)
	add("routes", d.RoutesAdded, d.RoutesRemoved)
	add("host setup", d.HostSetupAdded, d.HostSetupRemoved)
	add("devices", d.DevicesAdded, d.DevicesRemoved)
	for _, c := range []struct {
		what string
		extpkg.Change
	}{{"ui mode", d.UIMode}, {"isolation", d.Isolation}} {
		if c.Changed() {
			lines = append(lines, fmt.Sprintf("  ~ %s: %s -> %s", c.what, c.From, c.To))
		}
	}
	if len(lines) == 1 {
		lines = append(lines, "  nothing it did not ask for before")
	}
	return strings.Join(lines, "\n")
}

// pluginRollback makes the previous version of an installed package
// current again; --restore-state puts its data back from the snapshot
// taken when it was upgraded.
func (o Options) pluginRollback(log *slog.Logger, args []string, stdout io.Writer) error {
	fs, p := pluginFlags("rollback")
	restore := fs.Bool("restore-state", false, "restore the data from the snapshot of the previous version")
	id, err := onePackageID(fs, "rollback", args)
	if err != nil {
		return err
	}
	h, _, done, err := o.openStoppedHost(p)
	if err != nil {
		return offlineOnly(err, id+"/rollback")
	}
	defer done()
	rb, err := h.Rollback(id, extpkg.RollbackOptions{Restore: *restore, By: "command line"})
	if err != nil {
		if codeText(err) == "PLUGIN_STATE_TOO_NEW" && !*restore {
			return fmt.Errorf("%w; plugin rollback %s --restore-state also restores the data", err, id)
		}
		return err
	}
	fmt.Fprintf(stdout, "rolled %s back from %s to %s\n", id, rb.From, rb.Manifest.Version)
	if rb.Restored != "" {
		fmt.Fprintf(stdout, "data restored from snapshot %s\n", rb.Restored)
	}
	log.Info("plugin rolled back", "id", id, "from", rb.From, "to", rb.Manifest.Version, "restored", rb.Restored, "data", p.data)
	return nil
}
