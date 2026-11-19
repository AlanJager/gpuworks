package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/distribution"
	"gpuworks.ai/gpuworks/internal/server/extpkg/trust"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/version"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// editionName is the form an edition name takes.
var editionName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Options are what a build adds to the built-in command line. The zero value
// is the community server exactly as cmd/gpuworks runs it.
type Options struct {
	// Edition names the build: it is written to version.Edition before any
	// subcommand runs, and `gpuworks edition`, /healthz, the server build
	// info, the usage report and support bundles show it. Empty keeps the
	// built-in "community". A name that is not a lowercase word
	// ([a-z][a-z0-9-]{0,31}) makes Run panic, as a defect of the build.
	Edition string
	// Extensions are the build's compiled modules and console: serve runs
	// them, and migrate, reset-password, support-bundle and extra commands
	// open the database with their migrations. Each module's name is also
	// the server.yaml extensions section it reads.
	Extensions app.Extensions
	// Distribution is the edition's distribution channel: serve's update
	// checks, notices and catalog updates and the upgrade command read it.
	// The zero value is distribution.Community().
	Distribution distribution.Profile
	// PluginRoots are the project roots plugin publisher certificates and
	// trust updates chain to (key ids starting with "gwroot-"); nil is the
	// roots compiled into this build (trust.ProjectRoots). A fork's build
	// passes its own.
	PluginRoots []extapi.PublicKey
	// InstallHooks are host steps install, upgrade and uninstall run, in this
	// order (reversed on uninstall); see selfinstall.InstallHook.
	InstallHooks []selfinstall.InstallHook
	// Commands are extra subcommands, dispatched after the built-in ones and
	// listed in the usage after them, in this order. A name that a built-in
	// subcommand or another extra command already uses makes Run panic: two
	// commands answering one name is a defect of the build, not of the input.
	Commands []Command
}

// Command is one extra subcommand.
type Command struct {
	// Name is the first argument that selects the command.
	Name string
	// Usage is the synopsis printed after "gpuworks " in the usage, such as
	// "quota status [--data DIR]"; empty prints Name alone.
	Usage string
	// Run gets the arguments after Name. Its context is cancelled on SIGINT
	// and SIGTERM. An error other than flag.ErrHelp is logged as
	// "<name> failed" and exits 1.
	Run func(ctx context.Context, env Env, args []string) error
}

// Env is what Run hands an extra command.
type Env struct {
	Log            *slog.Logger
	Stdout, Stderr io.Writer
	// Extensions are the build's modules (Options.Extensions).
	Extensions app.Extensions
}

// OpenMaintenanceStore opens the database of an installation for a
// maintenance command: with the permission policy the built-in maintenance
// commands use, and with the build's extension migrations. dbPath empty is
// gpuworks.db in dataDir.
func (e Env) OpenMaintenanceStore(dataDir, dbPath string) (*store.Store, error) {
	if dbPath == "" {
		dbPath = filepath.Join(dataDir, "gpuworks.db")
	}
	policy, err := maintenanceDatabasePolicy(dbPath)
	if err != nil {
		return nil, err
	}
	return Options{Extensions: e.Extensions}.openMaintenanceStore(dbPath, policy)
}

// pluginRoots are the plugin roots of the build.
func (o Options) pluginRoots() []extapi.PublicKey {
	if o.PluginRoots != nil {
		return o.PluginRoots
	}
	return trust.ProjectRoots()
}

// validate panics when an extra command cannot be dispatched unambiguously,
// or when an install hook is missing.
func (o Options) validate() {
	if o.Edition != "" && !editionName.MatchString(o.Edition) {
		panic(fmt.Sprintf("cli: edition %q is not a lowercase name", o.Edition))
	}
	if !o.Distribution.IsZero() && !distribution.SameEdition(o.Distribution.Edition, o.Edition) {
		panic(fmt.Sprintf("cli: the distribution profile is for edition %q but the build is %q", distribution.NormalizeEdition(o.Distribution.Edition), distribution.NormalizeEdition(o.Edition)))
	}
	for n, hook := range o.InstallHooks {
		if hook == nil {
			panic(fmt.Sprintf("cli: install hook %d is nil", n+1))
		}
	}
	seen := map[string]bool{}
	for _, c := range o.Commands {
		switch {
		case strings.TrimSpace(c.Name) == "":
			panic("cli: an extra command has no name")
		case c.Run == nil:
			panic(fmt.Sprintf("cli: extra command %q has no Run", c.Name))
		case slices.Contains(builtinCommands, c.Name):
			panic(fmt.Sprintf("cli: extra command %q shadows a built-in subcommand", c.Name))
		case seen[c.Name]:
			panic(fmt.Sprintf("cli: extra command %q is registered twice", c.Name))
		}
		seen[c.Name] = true
	}
}

// applyEdition writes the build's edition before dispatch, so every
// subcommand and the server it starts report the same one.
func (o Options) applyEdition() {
	if o.Edition != "" {
		version.Edition = o.Edition
	}
}

// installDeps is the production install dependencies with the build's
// distribution profile.
func (o Options) installDeps(stderr io.Writer) selfinstall.Deps {
	deps := selfinstall.NewDeps(stderr)
	deps.Profile = o.Distribution
	return deps
}

// installer is the installer install and upgrade run: the build's profile
// and install hooks, after the core's own (installHooks), over the
// production dependencies.
func (o Options) installer(layout selfinstall.Layout, stderr io.Writer, streams *selfinstall.IO) *selfinstall.Installer {
	return selfinstall.New(layout, o.installDeps(stderr), streams).WithHooks(o.installHooks(layout)...)
}

// command is the extra command named name, or nil.
func (o Options) command(name string) *Command {
	for i := range o.Commands {
		if o.Commands[i].Name == name {
			return &o.Commands[i]
		}
	}
	return nil
}

// runExtra runs an extra command the way Run runs a built-in one.
func runExtra(c Command, log *slog.Logger, args []string, stderr io.Writer, ext app.Extensions) int {
	env := Env{Log: log, Stdout: os.Stdout, Stderr: stderr, Extensions: ext}
	if err := c.Run(cmdContext(), env, args); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Error(c.Name+" failed", "err", err)
		return 1
	}
	return 0
}

// extraUsage is the usage lines of the extra commands, each on a line of its
// own after the built-in ones; it is empty without extra commands, so the
// community usage stays as it was.
func extraUsage(cmds []Command) string {
	var b strings.Builder
	for _, c := range cmds {
		synopsis := c.Usage
		if synopsis == "" {
			synopsis = c.Name
		}
		b.WriteString("\n  gpuworks ")
		b.WriteString(synopsis)
	}
	return b.String()
}
