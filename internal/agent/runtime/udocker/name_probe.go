package udocker

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"
)

// createNameProbe is os.CreateTemp; tests replace it to play a filesystem
// that refuses ':' in file names.
var createNameProbe = os.CreateTemp

// colonNamesAllowed checks that dir's filesystem accepts ':' in a file name.
// The runtime cannot work without it: udocker's image store names every blob
// "sha256:<hex>", and the root filesystems it unpacks hold such names too
// (dpkg keeps /var/lib/dpkg/info/<package>:<arch>.list in every Debian-based
// image). RunPod's VAST-backed /workspace volume answers them with EINVAL.
func colonNamesAllowed(dir string) error {
	probe, err := createNameProbe(dir, ".gpuworks-name-probe:*")
	if err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EILSEQ) {
			return fmt.Errorf("%s does not allow ':' in file names, which container images need (for example /var/lib/dpkg/info/<package>:amd64.list); reinstall the agent with an install directory on a local filesystem", dir)
		}
		// Any other failure (a full disk, a directory this agent cannot write)
		// says nothing about file names; the operations that need the
		// directory report it themselves.
		slog.Warn("could not check whether file names may hold ':'", "dir", dir, "err", err)
		return nil
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}
