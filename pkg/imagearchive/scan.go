// Package imagearchive reads the repository tags of a container image archive
// (a `docker save` tar or an OCI-layout tar) that is published over plain HTTP,
// without downloading the archive.
//
// `docker save` writes `manifest.json` (and a legacy `repositories` entry) at
// the very end of the tar, so a Range request for the tail is enough to learn
// the real image references. OCI-layout archives instead carry `index.json`
// near the head, whose manifest descriptors may annotate the reference name.
//
// The package only uses the standard library and the caller-supplied
// *http.Client; it performs no other I/O.
package imagearchive

import (
	"bytes"
	"encoding/json"
	"strings"
)

const blockSize = 512

// refNameAnnotation is the OCI annotation carrying an image reference.
const refNameAnnotation = "org.opencontainers.image.ref.name"

// tarHeader is the subset of a ustar/GNU header block this package needs.
type tarHeader struct {
	name string
	size int64
}

// parseHeader validates one 512-byte block as a ustar/GNU tar header. It
// reports false for zero blocks, for blocks without the ustar magic and for
// blocks whose stored checksum does not match the computed one.
func parseHeader(b []byte) (tarHeader, bool) {
	if len(b) != blockSize {
		return tarHeader{}, false
	}
	magic := b[257:263]
	// "ustar\x00" (POSIX ustar) or "ustar " (GNU).
	if !bytes.Equal(magic, []byte("ustar\x00")) && !bytes.Equal(magic, []byte("ustar ")) {
		return tarHeader{}, false
	}
	if !checksumOK(b) {
		return tarHeader{}, false
	}
	name := cString(b[0:100])
	if prefix := cString(b[345:500]); prefix != "" && bytes.Equal(magic, []byte("ustar\x00")) {
		name = prefix + "/" + name
	}
	size, ok := parseNumeric(b[124:136])
	if !ok || size < 0 {
		return tarHeader{}, false
	}
	return tarHeader{name: name, size: size}, true
}

// checksumOK recomputes the header checksum with the checksum field blanked,
// accepting both the unsigned and the historical signed interpretation.
func checksumOK(b []byte) bool {
	stored, ok := parseNumeric(b[148:156])
	if !ok {
		return false
	}
	var unsigned int64
	var signed int64
	for i, c := range b {
		if i >= 148 && i < 156 {
			c = ' '
		}
		unsigned += int64(c)
		signed += int64(int8(c))
	}
	return stored == unsigned || stored == signed
}

// parseNumeric reads a NUL/space padded octal field, or a GNU base-256 field.
func parseNumeric(f []byte) (int64, bool) {
	if len(f) > 0 && f[0]&0x80 != 0 {
		// GNU base-256: the first byte's low bits plus the remaining bytes.
		var n int64
		if f[0]&0x40 != 0 { // negative values are never valid here
			return 0, false
		}
		n = int64(f[0] & 0x3f)
		for _, c := range f[1:] {
			n = n<<8 | int64(c)
			if n < 0 {
				return 0, false
			}
		}
		return n, true
	}
	s := strings.Trim(string(f), " \x00")
	if s == "" {
		return 0, true
	}
	var n int64
	for _, c := range s {
		if c < '0' || c > '7' {
			return 0, false
		}
		n = n*8 + int64(c-'0')
		if n < 0 {
			return 0, false
		}
	}
	return n, true
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// normalizeEntry strips the "./" prefix some writers emit.
func normalizeEntry(name string) string {
	return strings.TrimPrefix(strings.TrimPrefix(name, "./"), "/")
}

func padded(size int64) int64 {
	if r := size % blockSize; r != 0 {
		return size + blockSize - r
	}
	return size
}

// tailScan is the detailed result of a backwards scan.
type tailScan struct {
	tags []string
	// sawTar reports whether at least one valid tar header block was found in
	// the window, i.e. the window really is the tail of an uncompressed tar.
	sawTar bool
}

// needMore reports whether a larger tail window could still yield tags: the
// window is a tar tail, but manifest.json was not fully contained in it.
func (t tailScan) needMore() bool { return t.sawTar && len(t.tags) == 0 }

// ScanTail extracts repository tags from the tail of an uncompressed tar,
// which is where `docker save` writes manifest.json. data must end at the
// archive's last byte; its length need not be block aligned.
func ScanTail(data []byte) ([]string, bool) {
	r := scanTail(data)
	return r.tags, len(r.tags) > 0
}

func scanTail(data []byte) tailScan {
	// Tars are a whole number of 512-byte blocks, so align the window to the
	// archive's end rather than to its own start.
	if n := len(data) % blockSize; n != 0 {
		data = data[n:]
	}
	res := tailScan{sawTar: endsWithZeroBlocks(data)}
	for off := len(data) - blockSize; off >= 0; off -= blockSize {
		h, ok := parseHeader(data[off : off+blockSize])
		if !ok {
			continue
		}
		res.sawTar = true
		// docker writes the legacy "repositories" entry after manifest.json,
		// so keep walking backwards past anything else.
		if normalizeEntry(h.name) != "manifest.json" {
			continue
		}
		start := int64(off) + blockSize
		end := start + h.size
		if end > int64(len(data)) {
			// manifest.json straddles the window: a larger tail may help.
			return res
		}
		res.tags = dockerRepoTags(data[start:end])
		return res
	}
	return res
}

// endsWithZeroBlocks reports whether the window ends with the two zero blocks
// every tar writer emits, i.e. the window really is an uncompressed tar's end
// even when the last entry's payload fills the whole window.
func endsWithZeroBlocks(data []byte) bool {
	if len(data) < 2*blockSize {
		return false
	}
	for _, c := range data[len(data)-2*blockSize:] {
		if c != 0 {
			return false
		}
	}
	return true
}

// ScanHead extracts repository tags from the head of an uncompressed tar: the
// OCI-layout index.json (via its ref-name annotations) or, for archives that
// place it first, manifest.json. data must start at the archive's first byte.
func ScanHead(data []byte) ([]string, bool) {
	for off := 0; off+blockSize <= len(data); off += blockSize {
		h, ok := parseHeader(data[off : off+blockSize])
		if !ok {
			continue
		}
		start := int64(off) + blockSize
		end := start + h.size
		if end > int64(len(data)) {
			// The payload is cut off by the window, so the next header's
			// position is unknown. Stop instead of scanning into payload
			// bytes: layer.tar payloads are themselves tars and would yield
			// the wrong entries.
			break
		}
		switch normalizeEntry(h.name) {
		case "index.json":
			if tags := ociIndexTags(data[start:end]); len(tags) > 0 {
				return tags, true
			}
		case "manifest.json":
			if tags := dockerRepoTags(data[start:end]); len(tags) > 0 {
				return tags, true
			}
		}
		// Skip the payload so embedded layer tars are not mistaken for headers.
		next := start + padded(h.size)
		if next >= int64(len(data)) {
			break
		}
		off = int(next) - blockSize
	}
	return nil, false
}

// dockerRepoTags parses a `docker save` manifest.json.
func dockerRepoTags(b []byte) []string {
	var entries []struct {
		RepoTags []string `json:"RepoTags"`
	}
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil
	}
	var tags []string
	for _, e := range entries {
		tags = appendTags(tags, e.RepoTags...)
	}
	return tags
}

// ociIndexTags parses an OCI index.json, collecting the ref-name annotation of
// every manifest descriptor.
func ociIndexTags(b []byte) []string {
	var idx struct {
		Manifests []struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil
	}
	var tags []string
	for _, m := range idx.Manifests {
		tags = appendTags(tags, m.Annotations[refNameAnnotation])
	}
	return tags
}

// appendTags appends non-empty, not-yet-present tags, preserving order.
func appendTags(dst []string, tags ...string) []string {
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		dup := false
		for _, have := range dst {
			if have == t {
				dup = true
				break
			}
		}
		if !dup {
			dst = append(dst, t)
		}
	}
	return dst
}

// isGzip reports whether data starts with the gzip magic bytes.
func isGzip(data []byte) bool {
	return len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b
}
