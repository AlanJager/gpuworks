package selfinstall

import (
	"context"
	"fmt"
	"io"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/distribution"
)

// EditionCommand is the subcommand a release answers with its edition, on one
// line. A release from before editions does not know it and exits non-zero;
// it counts as the community edition, which is what every such release is.
const EditionCommand = "edition"

// EditionMismatchError is an upgrade that would replace the installation with
// another edition's build. The two editions may keep different data and run
// different install steps, so switching is never a side effect of an upgrade:
// `gpuworks upgrade` asks for --allow-edition-change, and the console refuses.
type EditionMismatchError struct {
	// Installed is the installed binary's edition, Release the new one's.
	Installed string
	Release   string
	ReleaseID string
	// Console marks an in-service upgrade, which has no way to allow it.
	Console bool
}

func (e *EditionMismatchError) Error() string {
	msg := fmt.Sprintf("release %s is the %s edition, but this installation runs the %s edition", e.ReleaseID, e.Release, e.Installed)
	if e.Console {
		return msg + "; switch editions with `gpuworks upgrade --archive <file> --allow-edition-change` on the server host"
	}
	return msg + "; pass --allow-edition-change to switch editions"
}

// binaryEdition asks a gpuworks binary for its edition. A binary that does
// not know EditionCommand, or answers with something that is not an edition
// name, predates editions and is community. Its complaint about the unknown
// subcommand is not the operator's business, so stderr is discarded.
func (i *Installer) binaryEdition(ctx context.Context, path string) string {
	out, err := i.deps.Run.Output(ctx, Command{Path: path, Args: []string{EditionCommand}, Stderr: io.Discard})
	if err != nil {
		return distribution.EditionCommunity
	}
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	line = strings.TrimSpace(line)
	if !distribution.ValidEdition(line) {
		return distribution.EditionCommunity
	}
	return distribution.NormalizeEdition(line)
}

// checkEdition compares the staged binary's edition with the installed one's
// and refuses a change unless allow is set. It runs before the service is
// touched, so a refusal changes nothing. An allowed change is said out loud:
// the release's own install steps belong to its binary, which this run is not.
func (i *Installer) checkEdition(ctx context.Context, staged, releaseID string, allow, console bool) error {
	installed := i.binaryEdition(ctx, i.layout.Binary())
	release := i.binaryEdition(ctx, staged)
	if installed == release {
		return nil
	}
	if !allow || console {
		return &EditionMismatchError{Installed: installed, Release: release, ReleaseID: releaseID, Console: console}
	}
	i.io.Warn("switching this installation from the %s edition to the %s edition (--allow-edition-change)", installed, release)
	i.io.Warn("afterwards run `%s install --dir %s` so the %s edition's own install steps are applied", i.layout.Binary(), i.layout.Dir, release)
	return nil
}
