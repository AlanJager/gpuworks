package cloudflared

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Request describes the cloudflared binary to acquire.
type Request struct {
	// BinDir is the controlled cache (<data-dir>/expose/bin).
	BinDir string
	// OSRelease is the content of /etc/os-release, empty to read the host's.
	OSRelease []byte
	GOARCH    string
	// Supported decides whether a binary's version may be used at all; nil
	// accepts anything. The named provider needs the connector's local config
	// endpoint, which was verified against one version — a PATH binary older
	// than that is skipped in favour of the controlled download rather than
	// being trusted.
	Supported func(version string) bool
	Progress  func(done, total int64)
	// Logf reports each step (downloading, extracting) to the server log.
	Logf func(format string, args ...any)
}

// Acquire returns a usable cloudflared binary and its version, preferring what
// the host already has: a compatible binary on PATH, then a verified cache
// entry, and only then a download. Nothing is ever installed system-wide — the
// server may not be root, and the system is not ours to change.
//
// The returned path is the immutable versioned cache file, never a shared
// symlink: a later download must not change which binary a running entry point
// is using.
func Acquire(ctx context.Context, client *http.Client, req Request) (string, string, error) {
	logf := req.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	supported := req.Supported
	if supported == nil {
		supported = func(string) bool { return true }
	}
	if path, err := exec.LookPath("cloudflared"); err == nil {
		if version, err := verifyBinary(ctx, path); err == nil {
			if supported(version) {
				return path, version, nil
			}
			logf("ignoring the cloudflared on PATH (%s): version %s is not one this feature has been verified against", path, version)
		} else {
			logf("ignoring the cloudflared on PATH (%s): %v", path, err)
		}
	}
	if path, version := cachedBinary(ctx, req.BinDir, supported); path != "" {
		return path, version, nil
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return "", "", fmt.Errorf("pkg.cloudflare.com publishes no package for %s; install a compatible cloudflared on the server's PATH", runtime.GOOS)
	}
	return download(ctx, client, req, logf)
}

// cachedBinary returns the newest cached binary whose version is acceptable.
func cachedBinary(ctx context.Context, binDir string, supported func(string) bool) (string, string) {
	entries, err := os.ReadDir(binDir)
	if err != nil {
		return "", ""
	}
	type candidate struct {
		path    string
		version string
	}
	var candidates []candidate
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "cloudflared-") {
			continue
		}
		version := versionFromCacheName(entry.Name())
		if version == "" || !supported(version) {
			continue
		}
		candidates = append(candidates, candidate{path: filepath.Join(binDir, entry.Name()), version: version})
	}
	sort.Slice(candidates, func(i, j int) bool { return compareVersions(candidates[i].version, candidates[j].version) > 0 })
	for _, c := range candidates {
		if verified, err := verifyBinary(ctx, c.path); err == nil {
			return c.path, verified
		}
	}
	return "", ""
}

// versionFromCacheName reads back "cloudflared-<version>-<arch>".
func versionFromCacheName(name string) string {
	rest := strings.TrimPrefix(name, "cloudflared-")
	idx := strings.LastIndex(rest, "-")
	if idx <= 0 {
		return ""
	}
	return rest[:idx]
}

// cacheName is the immutable name of one verified version.
func cacheName(version, goarch string) string {
	return fmt.Sprintf("cloudflared-%s-%s", version, goarch)
}

func download(ctx context.Context, client *http.Client, req Request, logf func(string, ...any)) (string, string, error) {
	goarch := req.GOARCH
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	if err := os.MkdirAll(req.BinDir, 0o2775); err != nil {
		return "", "", err
	}
	if runtime.GOOS == "darwin" {
		return downloadDarwin(ctx, client, req, goarch, logf)
	}

	osr := ParseOSRelease(req.OSRelease)
	if len(req.OSRelease) == 0 {
		if raw, err := os.ReadFile("/etc/os-release"); err == nil {
			osr = ParseOSRelease(raw)
		}
	}
	format, err := DetectFormat(FormatAuto, osr, hasCommand)
	if err != nil {
		return "", "", err
	}
	arch, err := ArchFor(goarch)
	if err != nil {
		return "", "", err
	}
	src, err := Resolve(ctx, client, format, arch)
	if err != nil {
		return "", "", fmt.Errorf("resolve the %s package: %w", format, err)
	}
	pkgPath := filepath.Join(req.BinDir, filepath.Base(src.URL))
	logf("downloading %s (%s, %.1f MB)", src.URL, format, float64(src.Size)/(1<<20))
	if err := Download(ctx, client, DownloadRequest{
		URL:      src.URL,
		Dest:     pkgPath,
		SHA256:   src.SHA256,
		Size:     src.Size,
		Progress: req.Progress,
	}); err != nil {
		return "", "", fmt.Errorf("download cloudflared (the link to pkg.cloudflare.com is slow but resumable; retry, or install a compatible cloudflared into the server's PATH): %w", err)
	}
	// The package carries the version; extract to a temporary name first, then
	// publish the immutable cache entry once the binary runs and reports which
	// version it is.
	staged := filepath.Join(req.BinDir, "cloudflared.extracted")
	logf("extracting %s", BinaryMember)
	if err := ExtractBinary(pkgPath, format, staged); err != nil {
		return "", "", err
	}
	version, err := verifyBinary(ctx, staged)
	if err != nil {
		_ = os.Remove(staged)
		return "", "", fmt.Errorf("the extracted binary did not run: %w", err)
	}
	final := filepath.Join(req.BinDir, cacheName(version, goarch))
	if err := os.Rename(staged, final); err != nil {
		_ = os.Remove(staged)
		return "", "", err
	}
	// The package is ~19 MB and only needed again for a failed extraction, which
	// has already been reported with the path in hand.
	_ = os.Remove(pkgPath)
	return final, version, nil
}

// downloadDarwin fetches Cloudflare's macOS build. It goes through the same
// GitHub release host that is unreachable from some networks (measured from a
// Chinese egress: the release asset host times out while pkg.cloudflare.com
// serves fine), so a failure here is reported with the manual-install path
// rather than retried forever — macOS has no package repository to fall back
// on.
func downloadDarwin(ctx context.Context, client *http.Client, req Request, goarch string, logf func(string, ...any)) (string, string, error) {
	assetArch := goarch
	url := fmt.Sprintf("https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-darwin-%s.tgz", assetArch)
	tgz := filepath.Join(req.BinDir, "cloudflared-darwin.tgz")
	logf("downloading %s", url)
	if err := Download(ctx, client, DownloadRequest{URL: url, Dest: tgz, Progress: req.Progress}); err != nil {
		return "", "", fmt.Errorf("download cloudflared for macOS: %w; install a compatible cloudflared into the server's PATH instead (for example `brew install cloudflared`)", err)
	}
	defer os.Remove(tgz)
	staged := filepath.Join(req.BinDir, "cloudflared.extracted")
	if err := ExtractDarwinTarball(tgz, staged); err != nil {
		return "", "", err
	}
	version, err := verifyBinary(ctx, staged)
	if err != nil {
		_ = os.Remove(staged)
		return "", "", fmt.Errorf("the extracted binary did not run: %w", err)
	}
	final := filepath.Join(req.BinDir, cacheName(version, goarch))
	if err := os.Rename(staged, final); err != nil {
		_ = os.Remove(staged)
		return "", "", err
	}
	return final, version, nil
}

// verifyBinary runs `<path> --version` and returns the reported version. The
// call is bounded on its own: a binary that hangs (a wrapper waiting on
// something, a wedged mount) must not hang the server with it.
func verifyBinary(ctx context.Context, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("no binary")
	}
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w (%s)", path, err, strings.TrimSpace(string(out)))
	}
	if version := parseVersionLine(string(out)); version != "" {
		return version, nil
	}
	return strings.TrimSpace(string(out)), nil
}

// parseVersionLine pulls the version out of "cloudflared version 2026.9.1
// (built 2026-09-11-13:35 UTC)".
func parseVersionLine(out string) string {
	fields := strings.Fields(out)
	for i, field := range fields {
		if field == "version" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// SupportsConfigEndpoint reports whether a version is one whose local config
// endpoint this feature has been verified against. 2026.9.1 is the version the
// endpoint, its version field and the ingress structure were checked against;
// an older PATH binary is skipped in favour of the controlled download instead
// of being trusted to behave the same.
func SupportsConfigEndpoint(version string) bool {
	return compareVersions(version, "2026.9.1") >= 0
}

func hasCommand(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
