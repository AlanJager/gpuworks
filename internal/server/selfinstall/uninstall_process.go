package selfinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (i *Installer) validateUninstallPID() error {
	raw, err := os.ReadFile(i.layout.PIDFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect pid file: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return fmt.Errorf("invalid pid file %s; inspect the running process before retrying", i.layout.PIDFile())
	}
	return nil
}

// A missing PID file is common after `stop`, but is not proof that no server
// remains. One process-table snapshot also catches a manual serve or an orphan
// running with a different address. Unmanaged processes are reported, never
// killed by name. Both Linux and macOS support this ps format.
func (i *Installer) checkUninstallProcesses(ctx context.Context) error {
	out, err := i.deps.Run.Output(ctx, Command{Path: "ps", Args: []string{"-ww", "-axo", "pid=,command="}})
	if err != nil {
		return fmt.Errorf("cannot confirm server processes exited: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return errors.New("cannot confirm server processes exited: ps returned an empty process table")
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		end := strings.IndexAny(line, " \t")
		if end < 0 {
			return fmt.Errorf("cannot parse process table entry %q", line)
		}
		pid, err := strconv.Atoi(line[:end])
		if err != nil || pid <= 0 {
			return fmt.Errorf("cannot parse process table entry %q", line)
		}
		if pid == os.Getpid() {
			continue // uninstall may be executing the installed binary itself
		}
		command := strings.TrimSpace(line[end:])
		ours, known := uninstallExecutableIdentity(pid, i.layout.Binary())
		if ours {
			return fmt.Errorf("server pid %d still runs %s; stop this process before retrying uninstall", pid, i.layout.Binary())
		}
		if known {
			continue
		}
		// argv is only a fallback: an operator may invoke ./gpuworks, a PATH
		// entry or an alias, rather than the absolute installed name. Never
		// mistake an unidentifiable relative serve for proof of shutdown.
		// Check the complete target first: spaces in an installation path
		// are not argument separators in ps's unquoted command display.
		if args, matches := strings.CutPrefix(command, i.layout.Binary()+" "); matches && (args == "serve" || strings.HasPrefix(args, "serve ")) {
			return fmt.Errorf("server pid %d still runs %s; stop this process before retrying uninstall", pid, i.layout.Binary())
		}
		for offset := 0; offset < len(command); {
			n := strings.Index(command[offset:], " serve")
			if n < 0 {
				break
			}
			start := offset + n
			offset = start + len(" serve")
			if offset < len(command) && command[offset] != ' ' && command[offset] != '\t' {
				continue // server.py is not the serve subcommand
			}
			// ps does not quote argv. Check complete executable candidates,
			// including aliases with spaces, rather than splitting their names.
			program := command[:start]
			if filepath.IsAbs(program) {
				if sameUninstallExecutable(program, i.layout.Binary()) {
					return fmt.Errorf("server pid %d still runs %s; stop this process before retrying uninstall", pid, i.layout.Binary())
				}
			} else if len(strings.Fields(program)) == 1 || strings.ContainsRune(program, filepath.Separator) {
				return fmt.Errorf("cannot identify relative server command for pid %d (%s); stop it before uninstalling", pid, command)
			}
		}
	}
	return nil
}

func sameUninstallExecutable(path, binary string) bool {
	// Linux preserves the old path after an executable is unlinked.
	path = strings.TrimSuffix(path, " (deleted)")
	if filepath.Clean(path) == filepath.Clean(binary) {
		return true
	}
	actual, err := os.Stat(path)
	if err != nil {
		return false
	}
	target, err := os.Stat(binary)
	return err == nil && os.SameFile(actual, target)
}
