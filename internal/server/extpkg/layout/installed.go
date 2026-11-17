package layout

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/extpkg/archive"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Hasher hashes installed package files, remembering each file's SHA-256
// under its (size, modification time, inode). A check before each start
// rehashes only files whose identity changed; the check at boot rehashes
// everything (full). It is safe for concurrent use.
type Hasher struct {
	mu     sync.Mutex
	known  map[string]fileHash
	hashed int // files read in full, for tests
}

type fileHash struct {
	id  fileID
	sum string
}

type fileID struct {
	size  int64
	mtime int64
	inode uint64
}

// NewHasher returns an empty Hasher.
func NewHasher() *Hasher { return &Hasher{known: map[string]fileHash{}} }

// Sum returns the SHA-256 of the regular file at path, from the cache when
// full is false and the file's identity is unchanged.
func (h *Hasher) Sum(path string, info fs.FileInfo, full bool) (string, error) {
	id := fileID{size: info.Size(), mtime: info.ModTime().UnixNano(), inode: inodeOf(info)}
	h.mu.Lock()
	cached, ok := h.known[path]
	h.mu.Unlock()
	if ok && !full && cached.id == id {
		return cached.sum, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	d := sha256.New()
	if _, err := io.Copy(d, f); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(d.Sum(nil))
	h.mu.Lock()
	h.known[path] = fileHash{id: id, sum: sum}
	h.hashed++
	h.mu.Unlock()
	return sum, nil
}

// ReadInstalled reads an installed package back into the shape a package
// archive reads to, so it can be verified again: the signature members and
// every other file with its size and SHA-256. A link or special file in
// the directory is refused as it would be in an archive.
func ReadInstalled(dir string, h *Hasher, full bool) (*archive.Contents, error) {
	c := &archive.Contents{}
	sig := map[string]*[]byte{
		archive.ManifestMember: &c.Manifest, archive.SignatureMember: &c.Signature, archive.CertificatesMember: &c.Certificates,
	}
	limits := map[string]int64{
		archive.ManifestMember: extapi.MaxEnvelopePayload, archive.SignatureMember: extapi.MaxEnvelopeBytes,
		archive.CertificatesMember: extapi.MaxCertBundle * extapi.MaxEnvelopeBytes,
	}
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if e.IsDir() {
			return nil
		}
		if !e.Type().IsRegular() {
			reason := archive.ReasonSpecialFile
			if e.Type()&fs.ModeSymlink != 0 {
				reason = archive.ReasonLink
			}
			return messages.NewProductError(messages.ErrorPluginPackageMalformed,
				fmt.Sprintf("installed package member %q is not a regular file", rel),
				map[string]any{"member": rel, "reason": reason})
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if dst, ok := sig[rel]; ok {
			if info.Size() > limits[rel] {
				return messages.NewProductError(messages.ErrorPluginPackageTooLarge, "installed signature member too large",
					map[string]any{"member": rel, "max": limits[rel]})
			}
			body, err := os.ReadFile(p)
			*dst = body
			return err
		}
		sum, err := h.Sum(p, info, full)
		if err != nil {
			return err
		}
		c.Members = append(c.Members, archive.Member{Path: rel, Size: info.Size(), SHA256: sum})
		return nil
	})
	if err != nil {
		return nil, err
	}
	for name, dst := range sig {
		if *dst == nil {
			return nil, messages.NewProductError(messages.ErrorPluginPackageMalformed,
				fmt.Sprintf("installed package lacks %s", name), map[string]any{"member": name, "reason": archive.ReasonMissing})
		}
	}
	sort.Slice(c.Members, func(i, j int) bool { return c.Members[i].Path < c.Members[j].Path })
	return c, nil
}

// Hashed is how many files h has read in full.
func (h *Hasher) Hashed() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hashed
}
