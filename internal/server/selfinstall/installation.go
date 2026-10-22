package selfinstall

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/launchd"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// Run modes of an installed server: kept running by a service manager
// (systemd, or launchd on macOS) that also starts it at boot, or by the
// detached supervisor (serve --supervise), which nothing starts at boot.
const (
	ModeSystemd = "systemd"
	ModeLaunchd = "launchd"
	ModeNohup   = "nohup"
)

// installation is an installed server as the machine shows it: server.yaml,
// the systemd unit or launch daemon plist, and the installed binary. Nothing records it a second time,
// so nothing can disagree with it.
type installation struct {
	Config      serverconfig.File
	Mode        string
	ServiceUser string
	// ReleaseID is what the installed binary reports; "" when it cannot run.
	ReleaseID string
	// BuiltAt is the build time the installed binary reports.
	BuiltAt string
	// FromInstallJSON marks an installation made before server.yaml: Config
	// was read from its install.json, and its unit (if any) still passes the
	// settings as environment variables.
	FromInstallJSON bool
}

// ErrNotInstalled reports that the directory holds neither server.yaml nor
// the install.json of an installation made before it.
var ErrNotInstalled = errors.New("no gpuworks installation found")

// readInstallation loads the installation in the layout's directory.
func (i *Installer) readInstallation(ctx context.Context) (installation, error) {
	inst := installation{Mode: ModeNohup}
	cfg, err := serverconfig.Load(i.layout.ConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		cfg, err = readInstallJSON(i.layout.installJSONPath())
		inst.FromInstallJSON = err == nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return installation{}, fmt.Errorf("%w at %s; run `gpuworks install --dir %s` first to adopt it",
			ErrNotInstalled, i.layout.Dir, i.layout.Dir)
	}
	if err != nil {
		return installation{}, err
	}
	inst.Config = cfg
	if unit, err := os.ReadFile(i.layout.UnitPath()); err == nil && unitRuns(unit, i.layout.Binary()) {
		inst.Mode = ModeSystemd
		inst.ServiceUser = unitUser(unit)
	} else if plist, err := os.ReadFile(i.layout.LaunchdPath()); err == nil {
		// Like the unit, a plist counts only when it runs this layout's binary.
		if spec, err := launchd.ReadSpec(plist); err == nil && spec.Args[0] == i.layout.Binary() {
			inst.Mode = ModeLaunchd
			inst.ServiceUser = spec.User
		}
	}
	build := i.installedBuild(ctx)
	inst.ReleaseID, inst.BuiltAt = build.ReleaseID, build.BuiltAt
	return inst, nil
}

// adoptLayout points the layout at the data and log directories the
// installation uses, unless the command line named others.
func (i *Installer) adoptLayout(cfg serverconfig.File, dataDirSet, logsDirSet bool) {
	if !dataDirSet && strings.TrimSpace(cfg.DataDir) != "" {
		i.layout.DataDir = cfg.DataDir
	}
	if !logsDirSet && strings.TrimSpace(cfg.Log.File) != "" {
		i.layout.LogsDir = filepath.Dir(cfg.Log.File)
	}
}

// unitRuns reports whether a unit starts this layout's binary, so a unit left
// by another installation on the same host is not mistaken for this one.
func unitRuns(unit []byte, binary string) bool {
	for _, line := range unitLines(unit) {
		if rest, ok := strings.CutPrefix(line, "ExecStart="); ok {
			return firstUnitWord(rest) == binary
		}
	}
	return false
}

// unitUser is the account a unit runs as; "" means root.
func unitUser(unit []byte) string {
	for _, line := range unitLines(unit) {
		if rest, ok := strings.CutPrefix(line, "User="); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// firstUnitWord undoes systemdQuote for the first word of a command line.
func firstUnitWord(line string) string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, `"`) {
		word, _, _ := strings.Cut(line, " ")
		return strings.ReplaceAll(word, "%%", "%")
	}
	var b strings.Builder
	for i := 1; i < len(line); i++ {
		switch c := line[i]; {
		case c == '\\' && i+1 < len(line):
			i++
			b.WriteByte(line[i])
		case c == '"':
			return strings.ReplaceAll(b.String(), "%%", "%")
		default:
			b.WriteByte(c)
		}
	}
	return ""
}

func unitLines(unit []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(unit))
	for sc.Scan() {
		out = append(out, strings.TrimSpace(sc.Text()))
	}
	return out
}

// installedRelease asks the installed binary for its release id.
func (i *Installer) installedBuild(ctx context.Context) binaryBuild {
	build, err := i.binaryBuild(ctx, i.layout.Binary())
	if err != nil {
		return binaryBuild{}
	}
	return build
}

// binaryBuild is what a gpuworks binary says about itself.
type binaryBuild struct {
	ReleaseID, Commit, BuiltAt string
}

// binaryBuild runs `<path> version`. A binary that runs but answers in another
// shape yields an empty build rather than an error.
func (i *Installer) binaryBuild(ctx context.Context, path string) (binaryBuild, error) {
	out, err := i.deps.Run.Output(ctx, Command{Path: path, Args: []string{"version"}})
	if err != nil {
		return binaryBuild{}, err
	}
	return parseVersionLine(out), nil
}

// parseVersionLine reads "gpuworks <release> (<commit>, <built>)".
func parseVersionLine(out string) binaryBuild {
	fields := strings.Fields(out)
	if len(fields) < 2 || fields[0] != "gpuworks" || strings.HasPrefix(fields[1], "(") {
		return binaryBuild{}
	}
	build := binaryBuild{ReleaseID: fields[1]}
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "gpuworks "+fields[1]))
	if inner, ok := strings.CutPrefix(rest, "("); ok {
		inner, _, _ = strings.Cut(inner, ")")
		commit, built, _ := strings.Cut(inner, ",")
		build.Commit, build.BuiltAt = strings.TrimSpace(commit), strings.TrimSpace(built)
	}
	return build
}

// listenOf is the listen section install writes: an explicit --addr, else
// what the installation already binds, else the address the default route
// leaves from.
func (i *Installer) listenOf(req InstallRequest, previous serverconfig.File) (serverconfig.Listen, error) {
	if req.AddrSet {
		return listenForAddr(req.Addr)
	}
	if !previous.Listen.Empty() {
		return previous.Listen, nil
	}
	ip, err := i.deps.detectIP()
	if err != nil {
		return serverconfig.Listen{}, err
	}
	return serverconfig.Listen{Console: []string{net.JoinHostPort(ip.String(), serverconfig.DefaultPort)}}, nil
}

// listenForAddr turns an --addr value into a listen section. A bare port or an
// empty host means every address.
func listenForAddr(addr string) (serverconfig.Listen, error) {
	host, port, err := splitAddr(addr)
	if err != nil {
		return serverconfig.Listen{}, err
	}
	if host == "" {
		host = "0.0.0.0"
	}
	l := serverconfig.Listen{Console: []string{net.JoinHostPort(host, port)}}
	if err := l.Validate(); err != nil {
		return serverconfig.Listen{}, fmt.Errorf("invalid --addr: %w", err)
	}
	return l, nil
}

// probeAddr is the address the health check reaches: 127.0.0.1 is bound on
// every port the server uses, or covered by a wildcard.
func probeAddr(l serverconfig.Listen) string {
	b := serverconfig.Bindings(l)
	for _, x := range b {
		ip, port, err := serverconfig.SplitAddr(x.Addr)
		if err == nil && (ip.IsUnspecified() || serverconfig.ServesEverything(ip)) {
			if ip.To4() == nil {
				return net.JoinHostPort("::1", port)
			}
			return net.JoinHostPort("127.0.0.1", port)
		}
	}
	return b[0].Addr
}
