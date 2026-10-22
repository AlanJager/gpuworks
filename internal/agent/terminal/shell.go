package terminal

import (
	"os"
	"os/user"
	goruntime "runtime"
	"strconv"
)

// shellCandidates is probed in order. bash first for a usable interactive
// experience; /bin/sh is the POSIX guarantee and the branch that actually runs
// on the trimmed Ascend and PPU base images. macOS has defaulted to zsh since
// 10.15: its /bin/bash is 3.2 and greets every session with a notice to switch.
var shellCandidates = func() []string {
	if goruntime.GOOS == "darwin" {
		return []string{"/bin/zsh", "/bin/bash", "/bin/sh"}
	}
	return []string{"/bin/bash", "/usr/bin/bash", "/bin/sh"}
}()

// detectShell picks the login shell to run.
//
// The agent's own $SHELL is deliberately ignored: under systemd it is usually
// absent or /bin/false, so trusting it would break the common install path.
func detectShell() string {
	for _, sh := range shellCandidates {
		if st, err := os.Stat(sh); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return sh
		}
	}
	return "/bin/sh"
}

// loginArgv runs shell as a login shell. On GPU nodes this matters: conda and
// CUDA generally put themselves on PATH from /etc/profile.d/*.sh, and skipping
// that yields the confusing case where nvcc works over SSH but not here.
func loginArgv(shell string) []string {
	return []string{shell, "-l"}
}

// hostIdentity describes the account the pty will run as.
type hostIdentity struct {
	Name string
	UID  int
	Home string
}

// currentIdentity resolves the agent's own account. With CGO_ENABLED=0, os/user
// parses /etc/passwd in pure Go; if that fails (container images without a
// passwd entry for the uid) the numeric uid is still reported honestly.
func currentIdentity() hostIdentity {
	uid := os.Getuid()
	id := hostIdentity{Name: strconv.Itoa(uid), UID: uid, Home: "/"}
	if u, err := user.Current(); err == nil {
		if u.Username != "" {
			id.Name = u.Username
		}
		if u.HomeDir != "" {
			id.Home = u.HomeDir
		}
	}
	return id
}

// buildEnv constructs the pty environment explicitly rather than inheriting the
// agent's. Under systemd the agent's environment is both sparse and polluted
// with GPUWORKS_* internals, neither of which belongs in an operator's shell.
//
// TERM and LANG are load-bearing: without TERM, vim/top/htop refuse to start or
// draw garbage, and without a UTF-8 locale both Chinese text and nvidia-smi's
// box-drawing characters come out mojibake.
func buildEnv(id hostIdentity, shell string, extra []string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	env := []string{
		"TERM=xterm-256color",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"HOME=" + id.Home,
		"USER=" + id.Name,
		"LOGNAME=" + id.Name,
		"SHELL=" + shell,
		"PATH=" + path,
	}
	return append(env, extra...)
}

// resolveCwd prefers the home directory and falls back to / when it is missing
// (a shell started in a nonexistent directory fails in confusing ways).
func resolveCwd(home string) string {
	if home != "" {
		if st, err := os.Stat(home); err == nil && st.IsDir() {
			return home
		}
	}
	return "/"
}
