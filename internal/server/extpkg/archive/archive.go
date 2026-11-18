// Package archive reads a plugin package (.gwpkg): a gzip-compressed tar
// whose signature members (manifest.json, manifest.sig, publisher.crt) it
// holds in memory and whose other members it streams to a Sink while it
// hashes them. It refuses links, special files, unsafe or repeated names
// and oversized packages before a byte of them reaches the sink's final
// place; whether the members are the ones the signed manifest lists is the
// verifier's question (docs/reference/plugin-protocol.md).
package archive

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Signature members: the signed manifest, its signatures and the
// publisher's certificates. They are never listed in the manifest.
const (
	ManifestMember     = "manifest.json"
	SignatureMember    = "manifest.sig"
	CertificatesMember = "publisher.crt"
)

// ImagesDir holds node image archives for offline sites; they count against
// Limits.MaxImageBytes instead of Limits.MaxBytes.
const ImagesDir = "node/images/"

// Default limits of a package.
const (
	DefaultMaxBytes      = 512 << 20
	DefaultMaxImageBytes = 64 << 30
	DefaultMaxMembers    = 8192
)

// maxTrailer bounds what may follow the tar's end-of-archive marker inside
// the compressed stream: the padding of the last tar record.
const maxTrailer = 64 << 10

// Limits bound what Read accepts.
type Limits struct {
	// MaxBytes bounds the members outside ImagesDir, summed.
	MaxBytes int64
	// MaxImageBytes bounds the image archives under ImagesDir, summed.
	MaxImageBytes int64
	// MaxMembers bounds the entries of the tar, directories included.
	MaxMembers int
}

// DefaultLimits are the limits of docs/reference/plugin-protocol.md.
func DefaultLimits() Limits {
	return Limits{MaxBytes: DefaultMaxBytes, MaxImageBytes: DefaultMaxImageBytes, MaxMembers: DefaultMaxMembers}
}

// Member is one member the sink received.
type Member struct {
	Path   string
	Size   int64
	SHA256 string
}

// Contents is what Read found: the signature members' bytes and every
// other member, sorted by path.
type Contents struct {
	Manifest     []byte
	Signature    []byte
	Certificates []byte
	Members      []Member
}

// Sink receives the bytes of every member other than the signature members.
type Sink interface {
	// Create opens path (a validated relative path) for writing.
	Create(path string) (io.WriteCloser, error)
}

// Discard is a Sink that keeps nothing: Read then only hashes.
type Discard struct{}

// Create returns a writer that drops everything.
func (Discard) Create(string) (io.WriteCloser, error) { return nopCloser{io.Discard}, nil }

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// Reasons of ErrorPluginPackageMalformed.
const (
	ReasonNotGzipTar     = "not_gzip_tar"
	ReasonLink           = "link"
	ReasonSpecialFile    = "special_file"
	ReasonUnsafePath     = "unsafe_path"
	ReasonDuplicate      = "duplicate_member"
	ReasonMissing        = "missing_member"
	ReasonTooManyMembers = "too_many_members"
)

func malformed(member, reason string) error {
	return messages.NewProductError(messages.ErrorPluginPackageMalformed,
		fmt.Sprintf("plugin package is malformed at %q: %s", member, reason),
		map[string]any{"member": member, "reason": reason})
}

func tooLarge(member string, max int64) error {
	return messages.NewProductError(messages.ErrorPluginPackageTooLarge,
		fmt.Sprintf("plugin package member %q takes it past %d bytes", member, max),
		map[string]any{"member": member, "max": max})
}

// signatureLimits bounds each signature member as pkg/extapi bounds its
// object, so an oversized one is refused before it is held in memory.
var signatureLimits = map[string]int64{
	ManifestMember:     extapi.MaxEnvelopePayload,
	SignatureMember:    extapi.MaxEnvelopeBytes,
	CertificatesMember: extapi.MaxCertBundle * extapi.MaxEnvelopeBytes,
}

// Read reads a package from r, streaming its members into sink. It fails
// with a coded error on the first problem; the sink may then hold partial
// files, which the caller discards.
func Read(r io.Reader, sink Sink, lim Limits) (*Contents, error) {
	br := bufio.NewReader(r)
	if magic, err := br.Peek(2); err != nil || magic[0] != 0x1f || magic[1] != 0x8b {
		return nil, malformed("", ReasonNotGzipTar)
	}
	gz, err := gzip.NewReader(br)
	if err != nil {
		return nil, malformed("", ReasonNotGzipTar)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	c := &Contents{}
	seen := map[string]bool{}
	tree := newPaths()
	var plain, images int64
	entries := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, malformed("", ReasonNotGzipTar)
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			// Archive-wide metadata (git archive writes one); the reader
			// applies none of it to the entries that follow.
			continue
		}
		entries++
		if entries > lim.MaxMembers {
			return nil, malformed(h.Name, ReasonTooManyMembers)
		}
		name, err := memberName(h)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, malformed(name, ReasonDuplicate)
		}
		seen[name] = true
		if err := tree.add(name, h.Typeflag == tar.TypeDir); err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if limit, ok := signatureLimits[name]; ok {
			body, err := readBounded(tr, name, h.Size, limit)
			if err != nil {
				return nil, err
			}
			switch name {
			case ManifestMember:
				c.Manifest = body
			case SignatureMember:
				c.Signature = body
			default:
				c.Certificates = body
			}
			continue
		}
		total, limit := &plain, lim.MaxBytes
		if strings.HasPrefix(name, ImagesDir) {
			total, limit = &images, lim.MaxImageBytes
		}
		if h.Size < 0 || h.Size > limit-*total {
			return nil, tooLarge(name, limit)
		}
		*total += h.Size
		m, err := copyMember(tr, sink, name, h.Size)
		if err != nil {
			return nil, err
		}
		c.Members = append(c.Members, m)
	}
	// Read the compressed stream to its end, so its checksum is verified;
	// only the tar's zero padding may follow its end-of-archive marker.
	if n, err := io.Copy(io.Discard, io.LimitReader(gz, maxTrailer+1)); err != nil || n > maxTrailer {
		return nil, malformed("", ReasonNotGzipTar)
	}
	for _, name := range []string{ManifestMember, SignatureMember, CertificatesMember} {
		if !seen[name] {
			return nil, malformed(name, ReasonMissing)
		}
	}
	sort.Slice(c.Members, func(i, j int) bool { return c.Members[i].Path < c.Members[j].Path })
	return c, nil
}

// paths tracks the member names case-folded, so that the members unpack to
// the same tree on a case-insensitive filesystem: no two names may fold to
// one, and no name may be both a file and a directory (a file "a" with a
// member "a/b", in either order).
type paths struct {
	names map[string]bool // every member, folded
	files map[string]bool // file members, folded
	dirs  map[string]bool // directories, explicit or implied by a member below
}

func newPaths() *paths {
	return &paths{names: map[string]bool{}, files: map[string]bool{}, dirs: map[string]bool{}}
}

func (p *paths) add(name string, dir bool) error {
	f := strings.ToLower(name)
	if p.names[f] {
		return malformed(name, ReasonDuplicate)
	}
	if p.files[f] || !dir && p.dirs[f] {
		return malformed(name, ReasonUnsafePath)
	}
	for i := strings.IndexByte(f, '/'); i >= 0; i = nextSlash(f, i) {
		if p.files[f[:i]] {
			return malformed(name, ReasonUnsafePath)
		}
		p.dirs[f[:i]] = true
	}
	p.names[f] = true
	if dir {
		p.dirs[f] = true
	} else {
		p.files[f] = true
	}
	return nil
}

func nextSlash(s string, i int) int {
	j := strings.IndexByte(s[i+1:], '/')
	if j < 0 {
		return -1
	}
	return i + 1 + j
}

// memberName validates an entry's type and name and returns the name
// without a directory's trailing slash.
func memberName(h *tar.Header) (string, error) {
	switch h.Typeflag {
	case tar.TypeReg, tar.TypeDir:
	case tar.TypeSymlink, tar.TypeLink:
		return "", malformed(h.Name, ReasonLink)
	default:
		return "", malformed(h.Name, ReasonSpecialFile)
	}
	name := h.Name
	if h.Typeflag == tar.TypeDir {
		name = strings.TrimSuffix(name, "/")
	}
	if !SafePath(name) {
		return "", malformed(h.Name, ReasonUnsafePath)
	}
	return name, nil
}

// SafePath reports whether p is a relative, slash-separated path with no
// empty, "." or ".." segment, no backslash, no NUL and at most 512 bytes.
func SafePath(p string) bool {
	if p == "" || len(p) > 512 || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return false
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func readBounded(r io.Reader, name string, size, limit int64) ([]byte, error) {
	if size < 0 || size > limit {
		return nil, tooLarge(name, limit)
	}
	var buf bytes.Buffer
	buf.Grow(int(size))
	if _, err := io.CopyN(&buf, r, size); err != nil {
		return nil, malformed(name, ReasonNotGzipTar)
	}
	return buf.Bytes(), nil
}

// sourceReader remembers whether a copy failed on its read side, so a
// truncated or corrupt archive is told apart from a sink that cannot write.
type sourceReader struct {
	r   io.Reader
	err error
}

func (s *sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		s.err = err
	}
	return n, err
}

func copyMember(r io.Reader, sink Sink, name string, size int64) (Member, error) {
	w, err := sink.Create(name)
	if err != nil {
		return Member{}, err
	}
	h := sha256.New()
	src := &sourceReader{r: r}
	_, copyErr := io.CopyN(io.MultiWriter(w, h), src, size)
	closeErr := w.Close()
	if copyErr != nil {
		if src.err != nil || errors.Is(copyErr, io.EOF) {
			return Member{}, malformed(name, ReasonNotGzipTar)
		}
		return Member{}, fmt.Errorf("write package member %s: %w", name, copyErr)
	}
	if closeErr != nil {
		return Member{}, fmt.Errorf("write package member %s: %w", name, closeErr)
	}
	return Member{Path: name, Size: size, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}
