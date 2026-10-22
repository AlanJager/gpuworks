package selfinstall

import (
	"context"
	"debug/elf"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// upgradeSource is where an upgrade takes its new release from: the
// distribution channel, a release site, or a release archive the operator
// already copied onto the machine. Either way the rest of the upgrade (stop,
// backup, replace, probe, rollback) is the same.
type upgradeSource struct {
	ReleaseID string
	Commit    string
	BuiltAt   string
	// Source is the distribution source or release site, empty for an archive.
	Source string
	// newerThan says whether the release is an update for the given running
	// release and build time; nil means any other release is.
	newerThan func(releaseID, builtAt string) bool

	// pinMismatch is the error for a --version this source does not carry.
	pinMismatch func(pin string) error
	// stage puts the new binary at stagedBinary and returns that path. It runs
	// only once the upgrade is going ahead, so --check downloads nothing.
	stage func(ctx context.Context) (string, error)
}

// stagedBinary is where the new binary waits until it replaces the installed
// one.
func (i *Installer) stagedBinary() string {
	return filepath.Join(i.layout.TmpDir(), binaryName+".new")
}

// siteSource reads the release a release site publishes. The artifact is
// fetched by stage and checked against the manifest's size and sha256. The
// manifest is unsigned: trust rests on the site's TLS, which is why this path
// is kept for the internal site and the channel is the default.
func (i *Installer) siteSource(ctx context.Context, baseURL string) (upgradeSource, error) {
	base, err := NormalizeBaseURL(baseURL)
	if err != nil {
		return upgradeSource{}, err
	}
	release, err := i.fetchRelease(ctx, base)
	if err != nil {
		return upgradeSource{}, err
	}
	goos, goarch := currentPlatform()
	asset, err := release.Asset(goos, goarch)
	if err != nil {
		return upgradeSource{}, err
	}
	return upgradeSource{
		ReleaseID: release.ReleaseID, Commit: release.Commit, BuiltAt: release.BuildAt, Source: base,
		pinMismatch: func(pin string) error {
			return fmt.Errorf("--version %s is not what %s publishes: the site is at %s", pin, base, release.ReleaseID)
		},
		stage: func(ctx context.Context) (string, error) {
			endpoint, err := assetURL(base, asset)
			if err != nil {
				return "", err
			}
			if err := mkdirAll(i.layout.TmpDir(), dirMode); err != nil {
				return "", err
			}
			i.io.Say("downloading %s", asset.Name)
			archive, err := i.downloadAsset(ctx, endpoint, asset, i.layout.TmpDir())
			if err != nil {
				return "", err
			}
			defer func() { _ = os.Remove(archive) }()
			staged := i.stagedBinary()
			if err := extractBinary(archive, staged); err != nil {
				return "", err
			}
			return staged, nil
		},
	}, nil
}

// archiveSource takes the release from a local archive. There is no manifest
// to say what it holds, so the binary is unpacked up front and asked: its
// answer is the release id the upgrade compares and records, and a binary
// that cannot run here (another architecture, a truncated file) is refused
// before the service is touched. Integrity rests on the archive's own gzip
// checksums; the release's SHA256SUMS is for the operator to check on arrival.
func (i *Installer) archiveSource(ctx context.Context, archive string) (upgradeSource, error) {
	path, err := filepath.Abs(strings.TrimSpace(archive))
	if err != nil {
		return upgradeSource{}, err
	}
	if info, err := os.Stat(path); err != nil {
		return upgradeSource{}, fmt.Errorf("--archive: %w", err)
	} else if !info.Mode().IsRegular() {
		return upgradeSource{}, fmt.Errorf("--archive %s is not a file", path)
	}
	if err := mkdirAll(i.layout.TmpDir(), dirMode); err != nil {
		return upgradeSource{}, err
	}
	i.io.Say("unpacking %s", path)
	staged := i.stagedBinary()
	if err := extractBinary(path, staged); err != nil {
		return upgradeSource{}, err
	}
	goos, goarch := currentPlatform()
	if err := checkBinaryMachine(staged, goarch); err != nil {
		return upgradeSource{}, fmt.Errorf("the gpuworks binary in %s is not for this %s/%s host: %w",
			filepath.Base(path), goos, goarch, err)
	}
	build, err := i.binaryBuild(ctx, staged)
	if err != nil {
		return upgradeSource{}, fmt.Errorf("the gpuworks binary in %s does not run on this %s/%s host: %w",
			filepath.Base(path), goos, goarch, err)
	}
	if err := checkArchiveVersion(build.ReleaseID); err != nil {
		return upgradeSource{}, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return upgradeSource{
		ReleaseID: build.ReleaseID, Commit: build.Commit, BuiltAt: build.BuiltAt,
		pinMismatch: func(pin string) error {
			return fmt.Errorf("--version %s is not what %s holds: the archive is at %s", pin, filepath.Base(path), build.ReleaseID)
		},
		stage: func(context.Context) (string, error) { return staged, nil },
	}, nil
}

// checkArchiveVersion refuses an archive whose binary has no release id, for
// the reason checkLocalVersion refuses such an installation: nothing would
// tell whether the upgrade worked or name the release it left behind. A local
// build is put in place by running its own `install`.
func checkArchiveVersion(id string) error {
	switch {
	case id == "", id == "dev", strings.HasSuffix(id, "-dirty"):
		return fmt.Errorf("its gpuworks binary carries no release id (%q, a local build); run its own `gpuworks install` to put a local build in place", id)
	}
	return nil
}

// elfMachines maps the Go architectures a release is built for to their ELF
// machine.
var elfMachines = map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}

// checkBinaryMachine refuses an ELF binary built for another architecture.
// Running it is not proof enough: a host with qemu binfmt handlers registered
// (common wherever multi-arch images are built) runs a foreign binary, slowly,
// and would have it installed as the server. Anything that is not ELF (or an
// architecture this table does not know) is left to the run that follows.
func checkBinaryMachine(path, goarch string) error {
	want, known := elfMachines[goarch]
	if !known {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	magic := make([]byte, len(elf.ELFMAG))
	if _, err := io.ReadFull(f, magic); err != nil || string(magic) != elf.ELFMAG {
		return nil
	}
	file, err := elf.NewFile(f)
	if err != nil {
		return err
	}
	if file.Machine != want {
		return fmt.Errorf("it is built for %s", file.Machine)
	}
	return nil
}
