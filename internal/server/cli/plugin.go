package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/server/expose"
	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/extpkg/catalog"
	"gpuworks.ai/gpuworks/internal/server/extpkg/compat"
	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/extpkg/trust"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

const pluginUsage = `  gpuworks plugin install --file PKG.gwpkg [--disabled]
  gpuworks plugin list
  gpuworks plugin verify ID
  gpuworks plugin enable ID
  gpuworks plugin disable ID
  gpuworks plugin upgrade --file PKG.gwpkg [--approve]   (server stopped)
  gpuworks plugin rollback ID [--restore-state]          (server stopped)
  gpuworks plugin uninstall ID [--purge] [--yes]
  gpuworks plugin state ID
  gpuworks plugin trust import FILE [--cert publisher.crt]
  gpuworks plugin index import FILE --cert publisher.crt
  gpuworks plugin compat
  gpuworks plugin host-setup [--id ID | --prune] [--service-user USER]   (as root)
  gpuworks plugin clock reset --to RFC3339 --reason TEXT   (as root, server stopped)
  gpuworks plugin pstate clear ID KIND --reason TEXT       (as root, server stopped)
                 (each also takes [--data DIR] [--db PATH] [--config server.yaml])`

// errCompatFindings is returned by plugin compat when it printed findings.
var errCompatFindings = errors.New("this server build cannot run everything in the data directory as it is")

// pluginCommand runs `gpuworks plugin ...`. Every subcommand works with the
// server stopped: it opens the database the way the other maintenance
// commands do, and the plugins directory under its own lock. enable,
// disable, upgrade, rollback and uninstall change what a running server
// keeps in memory, so they also require it to be stopped (requireServerStopped), as do the
// root-only clock reset and pstate clear, which record offline maintenance
// for the next boot to apply.
func (o Options) pluginCommand(log *slog.Logger, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		fmt.Fprintln(stdout, "usage:\n"+pluginUsage)
		return flag.ErrHelp
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "install":
		return o.pluginInstall(log, rest, stdout)
	case "list":
		return o.pluginList(rest, stdout)
	case "verify":
		return o.pluginVerify(rest, stdout)
	case "enable", "disable":
		return o.pluginSetEnabled(log, sub, rest, stdout)
	case "upgrade":
		return o.pluginUpgrade(log, rest, stdout)
	case "rollback":
		return o.pluginRollback(log, rest, stdout)
	case "uninstall", "remove": // remove is the earlier name, kept as an alias
		return o.pluginUninstall(log, sub, rest, stdout)
	case "state":
		return o.pluginState(rest, stdout)
	case "trust":
		if len(rest) == 0 || rest[0] != "import" {
			fmt.Fprintln(stdout, "usage:\n"+pluginUsage)
			return flag.ErrHelp
		}
		return o.pluginTrustImport(log, rest[1:], stdout)
	case "index":
		if len(rest) == 0 || rest[0] != "import" {
			fmt.Fprintln(stdout, "usage:\n"+pluginUsage)
			return flag.ErrHelp
		}
		return o.pluginIndexImport(log, rest[1:], stdout)
	case "compat":
		return o.pluginCompat(rest, stdout)
	case "host-setup":
		return o.pluginHostSetup(log, rest, stdout)
	case "clock":
		return o.pluginClock(log, rest, stdout)
	case "pstate":
		return o.pluginPstate(log, rest, stdout)
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, "usage:\n"+pluginUsage)
		return nil
	}
	return fmt.Errorf("unknown plugin subcommand %q", sub)
}

// pluginPaths are the locations a plugin subcommand works on.
type pluginPaths struct {
	data, db, config string
	// loaded is the plugins section openHost applied.
	loaded serverconfig.Plugins
}

func pluginFlags(name string) (*flag.FlagSet, *pluginPaths) {
	fs := flag.NewFlagSet("plugin "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	p := &pluginPaths{}
	fs.StringVar(&p.data, "data", "", "data directory (default: data_dir of server.yaml next to the binary, then of the default installation, then ./data)")
	fs.StringVar(&p.db, "db", "", "sqlite database path (default: gpuworks.db in the data directory)")
	fs.StringVar(&p.config, "config", "", "server.yaml whose plugins section to apply (default: next to the binary, then the default installation's)")
	return fs, p
}

// parseInterleaved parses flags given before or after positional arguments.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func (p *pluginPaths) resolve() {
	p.data, _ = expose.ResolveDataDir(p.data)
	if p.db == "" {
		p.db = filepath.Join(p.data, "gpuworks.db")
	}
}

// existing resolves p and refuses a data directory that does not exist: a
// mistyped --data must not create a second installation.
func (p *pluginPaths) existing() error {
	p.resolve()
	if info, err := os.Stat(p.data); err != nil || !info.IsDir() {
		return fmt.Errorf("data directory %s does not exist; pass --data with the server's data directory", p.data)
	}
	return nil
}

// plugins reads server.yaml's plugins section: --config when given (it must
// load), otherwise the first installation file that exists.
func (p *pluginPaths) plugins() (serverconfig.Plugins, error) {
	if p.config != "" {
		f, err := serverconfig.Load(p.config)
		return f.Plugins, err
	}
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, serverconfig.Path(filepath.Dir(exe)))
	}
	candidates = append(candidates, serverconfig.Path(selfinstall.DefaultDir))
	for _, path := range candidates {
		f, err := serverconfig.Load(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		return f.Plugins, err
	}
	return serverconfig.Plugins{}, nil
}

// openHost opens the database and the package host of p.
func (o Options) openHost(p *pluginPaths) (*extpkg.Host, *store.Store, error) {
	if err := p.existing(); err != nil {
		return nil, nil, err
	}
	cfg, err := p.plugins()
	if err != nil {
		return nil, nil, err
	}
	p.loaded = cfg
	policy, err := maintenanceDatabasePolicy(p.db)
	if err != nil {
		return nil, nil, err
	}
	st, err := o.openMaintenanceStore(p.db, policy)
	if err != nil {
		return nil, nil, fmt.Errorf("open plugin database %s: %w; use the installed gpuworks binary or run it with the service owner's privileges (sudo when required)", p.db, err)
	}
	h, err := extpkg.NewHost(extpkg.Setup{DataDir: p.data, Roots: o.pluginRoots(), Config: cfg, Store: st})
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	return h, st, nil
}

func (o Options) pluginTrustImport(log *slog.Logger, args []string, stdout io.Writer) error {
	fs, p := pluginFlags("trust import")
	certFile := fs.String("cert", "", "publisher.crt that certifies a publisher's own revocation list")
	files, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(files) != 1 {
		return errors.New("plugin trust import needs exactly one file")
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		return err
	}
	var certs []byte
	if *certFile != "" {
		if certs, err = os.ReadFile(*certFile); err != nil {
			return err
		}
	}
	h, st, err := o.openHost(p)
	if err != nil {
		return err
	}
	defer st.Close()
	in, err := trust.Import(st, h.Verifier.Trust, raw, certs)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "applied %s serial %d (%s): %d roots added, %d retired, %d revocations, %d floors\n",
		in.Kind, in.Serial, in.Issuer, in.AddedRoots, in.RetiredRoots, in.Revocations, in.Floors)
	log.Info("plugin trust imported", "kind", in.Kind, "issuer", in.Issuer, "serial", in.Serial)
	return nil
}

// pluginIndexImport applies a publisher's package index offline and prints
// which version of each package this server would install.
func (o Options) pluginIndexImport(log *slog.Logger, args []string, stdout io.Writer) error {
	fs, p := pluginFlags("index import")
	certFile := fs.String("cert", "", "the publisher's publisher.crt")
	files, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	if len(files) != 1 || *certFile == "" {
		return errors.New("plugin index import needs one index file and --cert publisher.crt")
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		return err
	}
	certs, err := os.ReadFile(*certFile)
	if err != nil {
		return err
	}
	h, st, err := o.openHost(p)
	if err != nil {
		return err
	}
	defer st.Close()
	in, err := catalog.Import(st, h.Verifier.Trust, h.Dir, raw, certs)
	if err != nil {
		return err
	}
	// The floors and revocations the index carried apply to the choice.
	if h, err = extpkg.NewHost(extpkg.Setup{DataDir: p.data, Roots: o.pluginRoots(), Config: p.loaded, Store: st}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "imported the index of %s, serial %d\n", in.Index.Publisher, in.Index.Serial)
	for _, c := range catalog.Choose(in.Index, h.Verifier.Offered, h.Verifier.Trust) {
		best := "none"
		if c.Best != nil {
			best = c.Best.Version
		}
		fmt.Fprintf(stdout, "%s: installs %s\n", c.ID, best)
		for _, r := range c.Newer {
			fmt.Fprintf(stdout, "  %s refused: %s (%s)\n", r.Entry.Version, r.Err, codeText(r.Err))
		}
	}
	log.Info("plugin index imported", "publisher", in.Index.Publisher, "serial", in.Index.Serial)
	return nil
}

// pluginCompat reports, without writing anything, whether this build can
// run the data directory: run it with the older binary before a downgrade.
func (o Options) pluginCompat(args []string, stdout io.Writer) error {
	fs, p := pluginFlags("compat")
	if _, err := parseInterleaved(fs, args); err != nil {
		return err
	}
	p.resolve()
	markers, err := store.ReadPluginMarkers(p.db)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read plugin state markers from %s: %w", p.db, err)
	}
	pkgs, err := compat.Installed(layout.New(p.data))
	if err != nil {
		return err
	}
	r := compat.Check(markers, compat.CoreMarkers(), extpkg.Capabilities(), pkgs)
	if r.OK() {
		fmt.Fprintf(stdout, "compatible: this build can start on %s and run its %d installed plugins\n", p.data, len(pkgs))
		return nil
	}
	for _, f := range r.Blocking {
		fmt.Fprintf(stdout, "refuses to start: %s: %s (%s)\n", f.Subject, f.Message, f.Code)
	}
	for _, f := range r.Packages {
		fmt.Fprintf(stdout, "plugin will not run: %s: %s (%s)\n", f.Subject, f.Message, f.Code)
	}
	return errCompatFindings
}

// codeText is an error's product code, or its text when it has none.
func codeText(err error) string {
	var pe *messages.ProductError
	if errors.As(err, &pe) {
		return pe.Code()
	}
	if code := extapi.CodeOf(err); code != "" {
		return code
	}
	return err.Error()
}
