// Package cloudflared acquires the cloudflared binary the expose feature runs.
//
// pkg.cloudflare.com is the only source that works from a Chinese egress:
// GitHub releases redirect to release-assets.githubusercontent.com, which is
// unreachable there, while the Debian and RPM repositories on Cloudflare's own
// domain serve fine. The packages are unpacked in pure Go (an ar reader for
// .deb, an RPM header walk plus cpio for .rpm), so no new dependency and no
// system package manager is involved — the extracted binary lands in the state
// directory and the system is never touched.
package cloudflared

import (
	"fmt"
	"strings"
)

// Format is the package format to fetch.
type Format string

const (
	// FormatAuto picks the format from /etc/os-release.
	FormatAuto Format = "auto"
	FormatDeb  Format = "deb"
	FormatRPM  Format = "rpm"
)

// Arch is a target architecture in each repository's spelling.
type Arch struct {
	GOARCH string
	// Deb is the name in the Debian pool path (and in the filename).
	Deb string
	// RPM is the name in the RPM repository path.
	RPM string
	// RPMName is the name embedded in the RPM path's filename.
	RPMName string
	// DebSupported is false for architectures the Debian repository does not
	// publish (386 exists only as an RPM).
	DebSupported bool
}

// ArchFor maps runtime.GOARCH to the repository spellings.
func ArchFor(goarch string) (Arch, error) {
	switch goarch {
	case "amd64":
		return Arch{GOARCH: "amd64", Deb: "amd64", RPM: "x86_64", RPMName: "x86_64", DebSupported: true}, nil
	case "arm64":
		return Arch{GOARCH: "arm64", Deb: "arm64", RPM: "aarch64", RPMName: "aarch64", DebSupported: true}, nil
	case "386":
		return Arch{GOARCH: "386", RPM: "386", RPMName: "386"}, nil
	default:
		return Arch{}, fmt.Errorf("cloudflared publishes no package for GOARCH=%s", goarch)
	}
}

// debIDs and rpmIDs are the os-release IDs whose package format we know.
// ID_LIKE is checked first because it is the field distributions set to name
// their family; ID alone covers the ones that only name themselves.
//
// Helix (the distribution used on dev1 and gpu3) is an RPM family system, so
// it is listed explicitly: its os-release ID_LIKE value has not been verified
// on a real host yet, and the explicit entry makes the choice correct either
// way.
var (
	debIDs = map[string]bool{
		"debian": true, "ubuntu": true, "linuxmint": true, "raspbian": true,
		"pop": true, "elementary": true, "kali": true, "deepin": true, "uos": true,
	}
	debLikes = map[string]bool{"debian": true, "ubuntu": true}

	rpmIDs = map[string]bool{
		"rhel": true, "redhat": true, "centos": true, "fedora": true,
		"rocky": true, "almalinux": true, "ol": true, "oracle": true,
		"openEuler": true, "openeuler": true, "anolis": true, "alinux": true,
		"tencentos": true, "kylin": true, "helix": true, "suse": true,
		"opensuse": true, "opensuse-leap": true, "sles": true,
	}
	rpmLikes = map[string]bool{"rhel": true, "fedora": true, "centos": true, "suse": true}
)

// OSRelease is the subset of /etc/os-release this package cares about.
type OSRelease struct {
	ID     string
	IDLike []string
	Pretty string
}

// ParseOSRelease reads the KEY=value lines, tolerating quotes and comments.
func ParseOSRelease(raw []byte) OSRelease {
	var out OSRelease
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(key) {
		case "ID":
			out.ID = value
		case "ID_LIKE":
			out.IDLike = strings.Fields(value)
		case "PRETTY_NAME":
			out.Pretty = value
		}
	}
	return out
}

// DetectFormat chooses the package format for this host. An explicit choice
// always wins; otherwise os-release decides, and if it names a distribution we
// do not know, the package managers actually installed are the last resort
// before giving up with an explanation the operator can act on.
func DetectFormat(explicit Format, osr OSRelease, hasCmd func(string) bool) (Format, error) {
	switch explicit {
	case FormatDeb, FormatRPM:
		return explicit, nil
	case FormatAuto, "":
	default:
		return "", fmt.Errorf("invalid package format %q: use auto, deb or rpm", explicit)
	}
	for _, like := range osr.IDLike {
		if debLikes[like] {
			return FormatDeb, nil
		}
		if rpmLikes[like] {
			return FormatRPM, nil
		}
	}
	if debIDs[osr.ID] {
		return FormatDeb, nil
	}
	if rpmIDs[osr.ID] {
		return FormatRPM, nil
	}
	switch {
	case hasCmd != nil && hasCmd("dpkg") && !hasCmd("rpm"):
		return FormatDeb, nil
	case hasCmd != nil && hasCmd("rpm") && !hasCmd("dpkg"):
		return FormatRPM, nil
	}
	id := osr.ID
	if id == "" {
		id = "unknown"
	}
	return "", fmt.Errorf("cannot tell whether %s wants a .deb or an .rpm; pass --pkg-format deb|rpm (or --cloudflared-path with an existing binary)", id)
}
