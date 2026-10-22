// Package selfinstall implements the `gpuworks install` and `gpuworks upgrade`
// subcommands: it copies the running control-plane binary into place, writes a
// service definition, starts it, and can later replace it with a release pulled
// from the download site — with a backup it can roll back to.
//
// Everything that touches the outside world (processes, the network, the clock)
// goes through an injected field on Installer, so the whole flow is exercised
// hermetically in tests.
package selfinstall

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/launchd"
	"gpuworks.ai/gpuworks/internal/server/installcred"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

const (
	// DefaultDir is the control-plane install directory. It is a subdirectory of
	// the agent's /opt/gpuworks so both can run on one host without sharing a
	// state directory.
	DefaultDir = "/opt/gpuworks/server"
	// ServiceName is the systemd unit basename ("gpuworks.service").
	ServiceName = "gpuworks"
	// DefaultServiceUser runs the service when the operator does not opt out.
	// The control plane listens on a network port, so it should not be root.
	DefaultServiceUser = "gpuworks"

	historyFile = "install-history.jsonl"
	upgradeLock = ".upgrade.lock"
	pidFile     = "gpuworks.pid"
	binaryName  = "gpuworks"

	binaryMode = 0o755
	// The install tree is shared workspace between the service account and the
	// operator: group-writable directories with the setgid bit, so files created
	// by either account keep the service group and stay writable by both.
	dirMode = 0o2775

	// DefaultBackupKeep is how many pre-upgrade snapshots survive pruning.
	DefaultBackupKeep = 3
)

// Layout is where an installed server keeps its files. It mirrors the tree
// documented in docs/reference/install-layout.md (目录布局).
type Layout struct {
	Dir        string `json:"dir"` // install dir: binary, server.yaml, pid, backups, data, logs
	DataDir    string `json:"data_dir"`
	LogsDir    string `json:"logs_dir"`
	BackupsDir string `json:"backups_dir"`
	// UnitDir holds the systemd unit and LaunchdDir the launch daemon's plist;
	// tests point them at temporary directories.
	UnitDir    string `json:"unit_dir"`
	LaunchdDir string `json:"launchd_dir"`
}

// NewLayout fills in the derived directories. An empty dir means DefaultDir;
// empty data/logs dirs are placed inside dir. Every directory is made absolute
// against the working directory: the nohup service starts the binary with the
// install directory as its working directory, so a relative --dir would be
// resolved a second time there (gpuworks/server/gpuworks/server/gpuworks).
func NewLayout(dir, dataDir, logsDir string) Layout {
	dir = strings.TrimRight(strings.TrimSpace(dir), "/")
	if dir == "" {
		dir = DefaultDir
	}
	dir = absolutePath(dir)
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		dataDir = filepath.Join(dir, "data")
	}
	dataDir = absolutePath(dataDir)
	logsDir = strings.TrimSpace(logsDir)
	if logsDir == "" {
		logsDir = filepath.Join(dir, "logs")
	}
	logsDir = absolutePath(logsDir)
	return Layout{
		Dir:        dir,
		DataDir:    dataDir,
		LogsDir:    logsDir,
		BackupsDir: filepath.Join(dir, "backups"),
		UnitDir:    "/etc/systemd/system",
		LaunchdDir: launchd.DaemonDir,
	}
}

// DirOf is the installation directory the commands that act on an existing
// installation (upgrade, restart) use when --dir is not given: the directory
// of exe when exe is the binary installed there (it holds server.yaml, or the
// install.json of an installation made before it), DefaultDir otherwise. A
// host can carry more than one installation, and `/opt/b/gpuworks upgrade`
// silently upgrading /opt/gpuworks/server instead is the surprise this avoids.
func DirOf(exe string) string {
	if dir, ok := installationDirOf(exe); ok {
		return dir
	}
	return DefaultDir
}

// installationDirOf reports the installation exe is the installed binary of.
func installationDirOf(exe string) (string, bool) {
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if filepath.Base(exe) != binaryName {
		return "", false
	}
	dir := filepath.Dir(exe)
	if fileExists(serverconfig.Path(dir)) || fileExists(filepath.Join(dir, installJSONFile)) {
		return dir, true
	}
	return "", false
}

// RunningDir is DirOf for the running executable.
func RunningDir() string {
	exe, err := os.Executable()
	if err != nil {
		return DefaultDir
	}
	return DirOf(exe)
}

// Binary is the installed server binary (install copies the running executable
// here and upgrade replaces it atomically).
func (l Layout) Binary() string { return filepath.Join(l.Dir, binaryName) }

// ConfigPath is server.yaml, the installed server's configuration.
func (l Layout) ConfigPath() string { return serverconfig.Path(l.Dir) }

// PIDFile records the process started by the nohup fallback.
func (l Layout) PIDFile() string { return filepath.Join(l.Dir, pidFile) }

// LockPath guards a running upgrade against a concurrent one.
func (l Layout) LockPath() string { return filepath.Join(l.Dir, upgradeLock) }

// TmpDir stages downloads and extracted binaries so a failed upgrade never
// leaves a partial file where the service expects a program.
func (l Layout) TmpDir() string { return filepath.Join(l.Dir, "tmp") }

// LogFile is the server's rotating file log.
func (l Layout) LogFile() string { return filepath.Join(l.LogsDir, "gpuworks-server.log") }

// CredentialsPath is the first-start credential record read by the server.
func (l Layout) CredentialsPath() string { return installcred.CredentialsPath(l.DataDir) }

// HistoryPath is the append-only install/upgrade audit log.
func (l Layout) HistoryPath() string { return filepath.Join(l.DataDir, historyFile) }

// UnitPath is the systemd unit written for the systemctl install mode.
func (l Layout) UnitPath() string { return filepath.Join(l.UnitDir, ServiceName+".service") }

// LaunchdPath is the launch daemon plist written for the launchd install mode.
func (l Layout) LaunchdPath() string {
	return filepath.Join(l.LaunchdDir, launchd.ServerLabel+".plist")
}

// StderrLog takes what the server writes before its own log is open, and a
// panic, when launchd runs it.
func (l Layout) StderrLog() string { return filepath.Join(l.LogsDir, "gpuworks-server.stderr.log") }

// BackupPath names a pre-upgrade snapshot by the release it replaces and the
// time it was taken, so a directory listing reads as a version timeline.
func (l Layout) BackupPath(releaseID string, at time.Time) string {
	name := sanitizeReleaseID(releaseID)
	if name == "" {
		name = "unknown"
	}
	return filepath.Join(l.BackupsDir, name+"-"+at.UTC().Format("20060102T150405Z")+".tar.gz")
}

// sanitizeReleaseID keeps a release id usable as a file name. Release ids come
// from the download site, so they are not trusted to be path-safe; a leading
// dot is dropped as well, so a backup never hides from a plain directory
// listing.
func sanitizeReleaseID(id string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(id) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_', r == '+':
			b.WriteRune(r)
		}
	}
	return strings.TrimLeft(b.String(), ".")
}

// absolutePath resolves path against the working directory. Abs only fails
// when the working directory itself is gone; the cleaned path is then the best
// that can be said.
func absolutePath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}
