package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/store/pstate"
)

// The lifecycle of an installed package: install, list, verify, enable,
// disable, uninstall. enable, disable and uninstall change what a running
// server holds in memory (its package entries, its pushed state), which it
// would quietly undo, so they run only with the server stopped.

func (o Options) pluginInstall(log *slog.Logger, args []string, stdout io.Writer) error {
	fs, p := pluginFlags("install")
	file := fs.String("file", "", "the .gwpkg package to install")
	disabled := fs.Bool("disabled", false, "install without enabling the package")
	if _, err := parseInterleaved(fs, args); err != nil {
		return err
	}
	if *file == "" {
		return errors.New("plugin install needs --file PKG.gwpkg")
	}
	f, err := os.Open(*file)
	if err != nil {
		return err
	}
	defer f.Close()
	h, st, err := o.openHost(p)
	if err != nil {
		return err
	}
	defer st.Close()
	owners, err := installedFeatureOwners(h)
	if err != nil {
		return err
	}
	in, err := h.Install(f, extpkg.InstallOptions{Enable: !*disabled, ApprovedBy: "command line", Features: owners})
	if err != nil {
		return err
	}
	m := in.Manifest
	what := "installed"
	if in.Unchanged {
		what = "already installed"
	}
	fmt.Fprintf(stdout, "%s %s %s (publisher %s, %s trust)\n", what, m.ID, m.Version, m.Publisher, in.Trust)
	if in.Previous != "" {
		fmt.Fprintf(stdout, "previous version %s kept for rollback\n", in.Previous)
	}
	fmt.Fprintf(stdout, "approved %s: permissions [%s] roles [%s]\n", in.ApprovalHash, strings.Join(m.Permissions, " "), strings.Join(m.Roles, " "))
	log.Info("plugin installed", "id", m.ID, "version", m.Version, "data", p.data, "enabled", !*disabled)
	return nil
}

// packageFeatureOwners maps each feature key an installed package declares
// to that package's id (extpkg.FeatureOwners). With the server stopped the
// compiled modules are not loaded, so the command line checks installed
// packages only; boot keeps a compiled module's keys either way.
type packageFeatureOwners map[string]string

func (p packageFeatureOwners) FeatureOwner(id, key string) string {
	if owner := p[key]; owner != id {
		return owner
	}
	return ""
}

// installedFeatureOwners reads the feature keys of the packages installed
// now. A key two packages declare stays with the lower id, as at boot.
func installedFeatureOwners(h *extpkg.Host) (packageFeatureOwners, error) {
	entries, err := h.List()
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b extpkg.Entry) int { return strings.Compare(a.ID, b.ID) })
	owners := packageFeatureOwners{}
	for _, e := range entries {
		if e.Verified == nil {
			continue
		}
		for _, f := range e.Verified.Manifest.Features {
			if _, taken := owners[f.Key]; !taken {
				owners[f.Key] = e.ID
			}
		}
	}
	return owners, nil
}

func (o Options) pluginList(args []string, stdout io.Writer) error {
	fs, p := pluginFlags("list")
	if _, err := parseInterleaved(fs, args); err != nil {
		return err
	}
	h, st, err := o.openHost(p)
	if err != nil {
		return err
	}
	defer st.Close()
	entries, err := h.List()
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintln(stdout, "no plugin installed")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tVERSION\tPUBLISHER\tTRUST\tENABLED\tSTATE")
	for _, e := range entries {
		publisher, trustClass, state := "-", "-", "ok"
		if v := e.Verified; v != nil {
			publisher, trustClass = v.Manifest.Publisher, v.Trust
			if v.Revoked != nil {
				state = "revoked"
			}
		} else {
			state = "broken: " + codeText(e.Err)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%t\t%s\n", e.ID, e.Version, publisher, trustClass, e.Control.Enabled, state)
	}
	return tw.Flush()
}

// onePackageID parses args and returns their one positional package id.
func onePackageID(fs *flag.FlagSet, name string, args []string) (string, error) {
	ids, err := parseInterleaved(fs, args)
	if err != nil {
		return "", err
	}
	if len(ids) != 1 {
		return "", fmt.Errorf("plugin %s needs exactly one package id", name)
	}
	return ids[0], nil
}

// openStoppedHost is openHost for a command that needs the server stopped:
// it holds the server lock until done is called, so no server starts
// meanwhile either.
func (o Options) openStoppedHost(p *pluginPaths) (h *extpkg.Host, st *store.Store, done func(), err error) {
	if err := p.existing(); err != nil {
		return nil, nil, nil, err
	}
	release, err := requireServerStopped(p.data)
	if err != nil {
		return nil, nil, nil, err
	}
	if h, st, err = o.openHost(p); err != nil {
		release()
		return nil, nil, nil, err
	}
	return h, st, func() { st.Close(); release() }, nil
}

// recheck verifies installed package id again, hashing every file. A
// package that no longer verifies, or whose signature is revoked, is
// PLUGIN_BROKEN, as the console reports it.
func recheck(h *extpkg.Host, id string) (*extpkg.Verified, error) {
	v, err := h.Recheck(id, true)
	if err != nil && codeText(err) == messages.ErrorPluginNotInstalled {
		return nil, err
	}
	if err == nil && v.Revoked != nil {
		err = errors.New("its signature is revoked")
	}
	if err != nil {
		return nil, messages.WrapProductError(messages.ErrorPluginBroken,
			fmt.Sprintf("plugin %s no longer verifies: %v (%s)", id, err, codeText(err)), map[string]any{"id": id}, err)
	}
	return v, nil
}

// pluginVerify verifies an installed package again and prints the result.
// It writes nothing, so it runs beside a live server.
func (o Options) pluginVerify(args []string, stdout io.Writer) error {
	fs, p := pluginFlags("verify")
	id, err := onePackageID(fs, "verify", args)
	if err != nil {
		return err
	}
	h, st, err := o.openHost(p)
	if err != nil {
		return err
	}
	defer st.Close()
	v, err := recheck(h, id)
	if err != nil {
		return err
	}
	m := v.Manifest
	fmt.Fprintf(stdout, "%s %s verifies (publisher %s, %s trust)\n", m.ID, m.Version, m.Publisher, v.Trust)
	return nil
}

// pluginSetEnabled runs enable or disable. Enable verifies the package
// first, as the console does, and refuses a broken one.
func (o Options) pluginSetEnabled(log *slog.Logger, sub string, args []string, stdout io.Writer) error {
	fs, p := pluginFlags(sub)
	id, err := onePackageID(fs, sub, args)
	if err != nil {
		return err
	}
	h, _, done, err := o.openStoppedHost(p)
	if err != nil {
		return err
	}
	defer done()
	enable := sub == "enable"
	if enable {
		if _, err := recheck(h, id); err != nil {
			return err
		}
	}
	if err := h.SetEnabled(id, enable); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%sd %s\n", sub, id)
	log.Info("plugin "+sub+"d", "id", id, "data", p.data)
	return nil
}

// pluginUninstall removes a package and everything it pushed, releasing its
// holds, so a later install of the same id starts clean. Running it again
// finishes an uninstall whose package went but whose rows did not.
func (o Options) pluginUninstall(log *slog.Logger, name string, args []string, stdout io.Writer) error {
	fs, p := pluginFlags(name)
	purge := fs.Bool("purge", false, "also move the plugin's data to the trash")
	id, err := onePackageID(fs, name, args)
	if err != nil {
		return err
	}
	h, st, done, err := o.openStoppedHost(p)
	if err != nil {
		return err
	}
	defer done()
	removeErr := h.Remove(id, *purge)
	if removeErr != nil && codeText(removeErr) != messages.ErrorPluginNotInstalled {
		return removeErr
	}
	ctx := context.Background()
	snap, err := st.LoadPushedState(ctx)
	if err != nil {
		return err
	}
	own := ownerState(snap, id)
	if removeErr != nil && own.empty() {
		return removeErr
	}
	if _, err := st.WritePushedState(ctx, &pstate.Batch{Uninstall: []string{id}}); err != nil {
		return fmt.Errorf("plugin %s is removed but what it pushed is not: %w; run uninstall again", id, err)
	}
	if removeErr == nil {
		fmt.Fprintf(stdout, "uninstalled %s\n", id)
	}
	fmt.Fprintf(stdout, "released what %s pushed: %d tables, %d node holds, %d overrides\n",
		id, len(own.Owners), len(own.Rows.NodeHolds), len(own.Overrides))
	log.Info("plugin uninstalled", "id", id, "purge", *purge, "data", p.data, "tables", len(own.Owners))
	return nil
}
