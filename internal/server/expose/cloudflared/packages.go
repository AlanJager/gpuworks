package cloudflared

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// BaseURL is Cloudflare's package host. The Debian pool path carries the
// version while the RPM path does not, which is why resolving a Debian package
// needs the index first. It is a variable so tests can point the repository at
// an httptest server.
var BaseURL = "https://pkg.cloudflare.com/cloudflared"

// PackageName is the package name in both repositories.
const PackageName = "cloudflared"

// BinaryMember is the path of the binary inside both packages.
const BinaryMember = "usr/bin/cloudflared"

// Source is one resolved download: where to get it and what it must hash to.
type Source struct {
	Format Format
	URL    string
	// SHA256 is the expected digest; both repositories publish one, so a
	// download is always verified.
	SHA256 string
	Size   int64
}

// DebIndexURL is the per-architecture Debian index.
func DebIndexURL(arch Arch) string {
	return fmt.Sprintf("%s/dists/any/main/binary-%s/Packages", BaseURL, arch.Deb)
}

// RPMFileURL is the RPM for an architecture. Cloudflare does not put the
// version in this filename, so it always resolves to the current release.
func RPMFileURL(arch Arch) string {
	return fmt.Sprintf("%s/rpm/%s/cloudflared-linux-%s.rpm", BaseURL, arch.RPM, arch.RPMName)
}

// RPMRepoMDURL is the repository metadata index, which points at the primary
// metadata holding the per-package checksums.
func RPMRepoMDURL() string { return BaseURL + "/rpm/repodata/repomd.xml" }

// DebEntry is one stanza of a Debian Packages index.
type DebEntry struct {
	Package  string
	Version  string
	Filename string
	SHA256   string
	Size     int64
}

// ParseDebPackages returns the newest stanza for pkg. The index is a sequence
// of stanzas separated by blank lines.
func ParseDebPackages(index []byte, pkg string) (*DebEntry, error) {
	var best *DebEntry
	for _, stanza := range strings.Split(strings.ReplaceAll(string(index), "\r\n", "\n"), "\n\n") {
		entry := &DebEntry{}
		for _, line := range strings.Split(stanza, "\n") {
			key, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			value = strings.TrimSpace(value)
			switch strings.TrimSpace(key) {
			case "Package":
				entry.Package = value
			case "Version":
				entry.Version = value
			case "Filename":
				entry.Filename = value
			case "SHA256":
				entry.SHA256 = value
			case "Size":
				entry.Size, _ = strconv.ParseInt(value, 10, 64)
			}
		}
		if entry.Package != pkg || entry.Filename == "" {
			continue
		}
		if best == nil || compareVersions(entry.Version, best.Version) > 0 {
			best = entry
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no %s package in the Debian index", pkg)
	}
	if best.SHA256 == "" {
		return nil, fmt.Errorf("%s %s has no SHA256 in the Debian index", pkg, best.Version)
	}
	return best, nil
}

// compareVersions orders two dotted versions by their numeric parts. Cloudflare
// publishes a calendar version (2026.9.1) with no epoch or revision, which is
// all this needs to handle.
func compareVersions(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var av, bv int
		if i < len(as) {
			av, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bv, _ = strconv.Atoi(bs[i])
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

var repoMDLocationRe = regexp.MustCompile(`<location href="([^"]*primary[^"]*)"`)

// ParseRepoMD returns the primary-metadata path from a repomd.xml.
func ParseRepoMD(raw []byte) (string, error) {
	m := repoMDLocationRe.FindSubmatch(raw)
	if m == nil {
		return "", fmt.Errorf("no primary metadata in %s", RPMRepoMDURL())
	}
	return string(m[1]), nil
}

var (
	packageBlockRe = regexp.MustCompile(`(?s)<package\b.*?</package>`)
	nameRe         = regexp.MustCompile(`<name>([^<]+)</name>`)
	archRe         = regexp.MustCompile(`<arch>([^<]+)</arch>`)
	checksumRe     = regexp.MustCompile(`<checksum[^>]*type="sha256"[^>]*>([0-9a-fA-F]{64})</checksum>`)
	locationRe     = regexp.MustCompile(`<location href="([^"]+)"`)
	sizeRe         = regexp.MustCompile(`<size package="(\d+)"`)
)

// ParsePrimaryXML returns the checksum and location of pkg for arch in a
// repository's primary metadata (already decompressed).
func ParsePrimaryXML(raw []byte, pkg, rpmArch string) (location, sha256 string, size int64, err error) {
	for _, block := range packageBlockRe.FindAll(raw, -1) {
		if m := nameRe.FindSubmatch(block); m == nil || string(m[1]) != pkg {
			continue
		}
		if m := archRe.FindSubmatch(block); m == nil || string(m[1]) != rpmArch {
			continue
		}
		if m := locationRe.FindSubmatch(block); m != nil {
			location = string(m[1])
		}
		if m := checksumRe.FindSubmatch(block); m != nil {
			sha256 = strings.ToLower(string(m[1]))
		}
		if m := sizeRe.FindSubmatch(block); m != nil {
			size, _ = strconv.ParseInt(string(m[1]), 10, 64)
		}
		if location != "" {
			return location, sha256, size, nil
		}
	}
	return "", "", 0, fmt.Errorf("no %s for %s in the RPM metadata", pkg, rpmArch)
}

// Resolve finds the download for this architecture, reading the repository
// metadata that carries the checksum. Both repositories publish one, so a
// resolved source is always verifiable.
func Resolve(ctx context.Context, client *http.Client, format Format, arch Arch) (*Source, error) {
	switch format {
	case FormatDeb:
		if !arch.DebSupported {
			return nil, fmt.Errorf("the Debian repository publishes no %s package; use --pkg-format rpm", arch.GOARCH)
		}
		index, err := getBytes(ctx, client, DebIndexURL(arch), 4<<20)
		if err != nil {
			return nil, err
		}
		entry, err := ParseDebPackages(index, PackageName)
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(entry.Filename, "pool/") || strings.Contains(entry.Filename, "..") {
			return nil, fmt.Errorf("refusing suspicious package path %q", entry.Filename)
		}
		return &Source{
			Format: FormatDeb,
			URL:    BaseURL + "/" + entry.Filename,
			SHA256: strings.ToLower(entry.SHA256),
			Size:   entry.Size,
		}, nil
	case FormatRPM:
		repomd, err := getBytes(ctx, client, RPMRepoMDURL(), 1<<20)
		if err != nil {
			return nil, err
		}
		primaryPath, err := ParseRepoMD(repomd)
		if err != nil {
			return nil, err
		}
		if strings.Contains(primaryPath, "..") {
			return nil, fmt.Errorf("refusing suspicious metadata path %q", primaryPath)
		}
		compressed, err := getBytes(ctx, client, BaseURL+"/rpm/"+primaryPath, 8<<20)
		if err != nil {
			return nil, err
		}
		decoded, err := decompressZstd(compressed)
		if err != nil {
			return nil, fmt.Errorf("decode RPM metadata: %w", err)
		}
		location, sha, size, err := ParsePrimaryXML(decoded, PackageName, arch.RPM)
		if err != nil {
			return nil, err
		}
		if strings.Contains(location, "..") {
			return nil, fmt.Errorf("refusing suspicious package path %q", location)
		}
		return &Source{
			Format: FormatRPM,
			URL:    BaseURL + "/rpm/" + location,
			SHA256: sha,
			Size:   size,
		}, nil
	default:
		return nil, fmt.Errorf("unknown package format %q", format)
	}
}

func decompressZstd(raw []byte) ([]byte, error) {
	dec, err := zstd.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return io.ReadAll(io.LimitReader(dec, 32<<20))
}

func getBytes(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	return body, nil
}
