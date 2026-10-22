package selfinstall

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
)

// A release site is what `gpuworks upgrade --base-url` reads: the internal
// site publishes <base>/data/release.json and the artifacts under
// <base>/downloads/, as scripts/package_release.sh lays them out. Without
// --base-url an upgrade reads the distribution channel instead (ADR 0040).
//
// Release paths on a release site.
const (
	releasePath  = "/data/release.json"
	downloadPath = "/downloads/"
)

// maxReleaseManifestBytes bounds the manifest read: it is a few hundred bytes,
// and a wrong URL that streams something endless must not fill memory.
const maxReleaseManifestBytes = 1 << 20

// ReleaseFile is one downloadable artifact.
type ReleaseFile struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`   // the site's card key, e.g. "macos-arm64"
	GoArch string `json:"goarch"` // the exact Go target
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Release is a release site's manifest.
type Release struct {
	ReleaseID string        `json:"release_id"`
	Commit    string        `json:"commit"`
	BuildAt   string        `json:"build_at"`
	Channel   string        `json:"channel"`
	Files     []ReleaseFile `json:"files"`
}

// NormalizeBaseURL validates and canonicalizes a release-site base URL.
func NormalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("--base-url needs a URL")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid --base-url %q: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("invalid --base-url %q: scheme must be http or https", raw)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("invalid --base-url %q: no host", raw)
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

// Asset picks the artifact for a target platform. goarch is matched exactly;
// the site's own arch key is only consulted for manifests that predate it.
func (r Release) Asset(goos, goarch string) (ReleaseFile, error) {
	for _, file := range r.Files {
		if file.OS == goos && file.GoArch == goarch {
			return file, nil
		}
	}
	for _, file := range r.Files {
		if file.OS == goos && strings.TrimPrefix(file.Arch, "macos-") == goarch && file.GoArch == "" {
			return file, nil
		}
	}
	return ReleaseFile{}, fmt.Errorf("release %s has no %s/%s build", r.ReleaseID, goos, goarch)
}

// assetURL is the download URL for an artifact, rejecting a name that could
// escape the downloads directory or name a directory entry rather than a file.
func assetURL(base string, file ReleaseFile) (string, error) {
	name := strings.TrimSpace(file.Name)
	if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, "/\\") ||
		strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("release manifest has an unusable artifact name %q", file.Name)
	}
	return base + downloadPath + name, nil
}

// fetchRelease reads and decodes the release manifest.
func (i *Installer) fetchRelease(ctx context.Context, base string) (Release, error) {
	endpoint := base + releasePath
	body, err := i.getBytes(ctx, endpoint, maxReleaseManifestBytes)
	if err != nil {
		return Release{}, fmt.Errorf("read the release manifest from %s: %w", endpoint, err)
	}
	var release Release
	if err := json.Unmarshal(body, &release); err != nil {
		return Release{}, fmt.Errorf("parse %s: %w", endpoint, err)
	}
	if strings.TrimSpace(release.ReleaseID) == "" {
		return Release{}, fmt.Errorf("%s carries no release_id", endpoint)
	}
	return release, nil
}

// getBytes performs a bounded GET.
func (i *Installer) getBytes(ctx context.Context, endpoint string, limit int64) ([]byte, error) {
	resp, err := i.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// get performs a GET and turns a non-2xx status into an error, so callers never
// parse an error page as if it were the resource they asked for.
func (i *Installer) get(ctx context.Context, endpoint string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := i.deps.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: status %d", endpoint, resp.StatusCode)
	}
	return resp, nil
}

// currentPlatform is the platform the running binary targets, which is the one
// an upgrade must fetch.
func currentPlatform() (string, string) { return runtime.GOOS, runtime.GOARCH }
