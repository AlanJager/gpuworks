package expose

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// The connector service is `gpuworks expose run`: the supervisor, running in a
// process of its own rather than inside the server. Restarting or upgrading
// the server leaves it alone, so a quick tunnel keeps its address. It runs
// until the entry point is disabled.
//
// `expose up` starts it as a background process in a session of its own, which
// is also outside the server's systemd cgroup; nothing starts it again after a
// reboot. `expose up --systemd` installs it as its own unit instead.

// SystemdUnitName is the connector service's unit under --systemd.
const SystemdUnitName = "gpuworks-expose.service"

// SystemdUnitPath is where that unit is written.
var SystemdUnitPath = "/etc/systemd/system/" + SystemdUnitName

// RunService hosts the entry point of store until it is disabled or ctx ends.
func RunService(ctx context.Context, store *Store, log *slog.Logger) error {
	held, err := store.SupervisorCheck()
	if err != nil {
		// Deliberately not "already running": nothing is. The lock could not be
		// examined, and the usual cause is a lock file another account created
		// (a root-run CLI under umask 022), which this service's account
		// cannot open.
		return fmt.Errorf("%w; fix the lock file's owner and mode", err)
	}
	if held {
		return fmt.Errorf("a connector service is already running for %s", store.Root())
	}
	sup := NewSupervisor(store, DepsFactory(), log)
	sup.exitWhenDisabled = true
	sup.Run(ctx)
	return nil
}

// ServiceCommand is how the connector service is started for a data dir.
func ServiceCommand(exe, dataDir string) []string {
	return []string{exe, "expose", "run", "--data", dataDir}
}

// ServiceAccount is the account the connector service runs as: the owner of
// the data dir, which is the server's own account in an installation. A
// connector started by root would otherwise run as root and leave files the
// server cannot replace. It is nil when the owner is the current user.
func ServiceAccount(dataDir string) (*user.User, error) {
	info, err := os.Stat(dataDir)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) == os.Geteuid() {
		return nil, nil
	}
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("%s belongs to uid %d: run this as that user or as root", dataDir, stat.Uid)
	}
	return user.LookupId(strconv.FormatUint(uint64(stat.Uid), 10))
}

// StartService launches the connector service as a background process: its
// own session, stdio on its log file, the data dir owner's account. It returns
// the pid; the process is not this one's child for long — Release lets it go.
func StartService(exe string, store *Store, dataDir string) (int, error) {
	account, err := ServiceAccount(dataDir)
	if err != nil {
		return 0, err
	}
	if err := store.Ensure(); err != nil {
		return 0, err
	}
	logFile, err := os.OpenFile(store.DaemonLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o664)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return 0, err
	}
	defer devNull.Close()
	args := ServiceCommand(exe, dataDir)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = "/"
	cmd.Env = os.Environ()
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	attr := &syscall.SysProcAttr{Setsid: true}
	if account != nil {
		uid, _ := strconv.ParseUint(account.Uid, 10, 32)
		gid, _ := strconv.ParseUint(account.Gid, 10, 32)
		attr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
		_ = os.Chown(store.DaemonLogPath(), int(uid), int(gid))
		cmd.Env = append(cmd.Env, "HOME="+account.HomeDir, "USER="+account.Username)
	}
	cmd.SysProcAttr = attr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

// RenderServiceUnit is the systemd unit for the connector service. It restarts
// on failure only: the service exits by itself, with success, once the entry
// point is disabled.
func RenderServiceUnit(exe, dataDir, account string) (string, error) {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=gpuworks public entry point (cloudflared)\n")
	b.WriteString("Wants=network-online.target\n")
	b.WriteString("After=network-online.target\n")
	b.WriteString("\n[Service]\n")
	b.WriteString("Type=simple\n")
	if account != "" {
		fmt.Fprintf(&b, "User=%s\n", account)
	}
	var words []string
	for _, arg := range ServiceCommand(exe, dataDir) {
		word, err := systemdWord(arg)
		if err != nil {
			return "", err
		}
		words = append(words, word)
	}
	fmt.Fprintf(&b, "ExecStart=%s\n", strings.Join(words, " "))
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=5\n")
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")
	return b.String(), nil
}

// systemdWord quotes one ExecStart word: systemd splits on whitespace and
// expands %-specifiers.
func systemdWord(arg string) (string, error) {
	if strings.ContainsAny(arg, "\n\"\\") {
		return "", fmt.Errorf("%q cannot be written into a systemd unit", arg)
	}
	arg = strings.ReplaceAll(arg, "%", "%%")
	if strings.ContainsAny(arg, " \t'") {
		return `"` + arg + `"`, nil
	}
	return arg, nil
}

// AbsDataDir is the data dir as the connector service must see it: an
// absolute path, because the service does not start in the operator's
// working directory.
func AbsDataDir(dataDir string) (string, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("the data directory %s does not exist", abs)
		}
		return "", err
	}
	return abs, nil
}
