package selfinstall

import (
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// SystemDir is the install directory on Linux: a subdirectory of the agent's
// /opt/gpuworks so both can run on one host without sharing a state directory.
const SystemDir = "/opt/gpuworks/server"

// homeInstallDir is the install directory on macOS, under the operator's home:
// a Mac is someone's own machine, and /opt is neither where its users look for
// files nor writable without administrator rights. It mirrors SystemDir inside
// ~/gpuworks, which the agent installs into on a Mac.
const homeInstallDir = "gpuworks/server"

// DefaultDir is the control-plane install directory when --dir is not given.
var DefaultDir = platformDefaultDir(runtime.GOOS, operatorHome)

func platformDefaultDir(goos string, home func() string) string {
	if goos == "darwin" {
		if h := home(); h != "" {
			return filepath.Join(h, homeInstallDir)
		}
	}
	return SystemDir
}

// operatorHome is the home directory of the person installing. Under sudo that
// is SUDO_USER's home, not root's: the installation belongs to the operator.
func operatorHome() string {
	if os.Geteuid() == 0 {
		if name := strings.TrimSpace(os.Getenv("SUDO_USER")); name != "" && name != "root" {
			if u, err := user.Lookup(name); err == nil && u.HomeDir != "" {
				return u.HomeDir
			}
		}
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return ""
}

// ExpandHome resolves a leading ~ or ~user the way a shell would. A directory
// typed at an install prompt, or passed as --dir=~/gpuworks, reaches the
// installer without a shell having expanded it, and would otherwise become a
// directory literally named "~" under the working directory.
func ExpandHome(path string) string {
	return expandHome(path, operatorHome, lookupHome)
}

func expandHome(path string, home func() string, lookup func(string) string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	name, rest, _ := strings.Cut(path[1:], "/")
	var dir string
	if name == "" {
		dir = home()
	} else {
		dir = lookup(name)
	}
	if dir == "" {
		return path
	}
	if rest == "" {
		return dir
	}
	return filepath.Join(dir, rest)
}

func lookupHome(name string) string {
	if u, err := user.Lookup(name); err == nil {
		return u.HomeDir
	}
	return ""
}
