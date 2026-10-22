package native

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	kgzip "github.com/klauspost/compress/gzip"
)

// sidecarStore holds plain-tar copies of large compressed layers, produced
// while those layers download.
//
// A pull is network bound and leaves the cores idle; a create is bound by
// inflating single DEFLATE streams, which cannot be split across cores. Writing
// the inflated form once during the pull moves that cost into time that is
// already being spent, so the later create only has to write files.
//
// The sidecar is an additional file and never replaces the blob it came from.
// That distinction matters: the layer store stays byte-identical to what the
// registry served, so digest verification, re-pulls, and handing the image back
// to udocker all keep working, and a missing or unreadable sidecar is only ever
// a lost optimization.
//
// It trades disk for time -- an inflated layer is a little over twice its
// compressed size -- so it is used only for layers big enough to matter and
// only while the filesystem has room to spare.
type sidecarStore struct {
	Dir      string
	Owner    *Owner
	MinSize  int64 // only sidecar layers at least this large
	Reserve  int64 // free space that must remain after writing
	MaxTotal int64 // cache size ceiling; 0 disables the cap
}

// inflatedSizeFactor estimates a layer's uncompressed size from its compressed
// size. Engine images measure about 2.4x overall and 2.6x for their largest
// layers; the rounded-up factor keeps the disk guard conservative.
const inflatedSizeFactor = 3

func (s *sidecarStore) path(digest string) string {
	return filepath.Join(s.Dir, strings.ReplaceAll(digest, ":", "-")+".tar")
}

// Get returns the sidecar for desc when one is present and complete.
func (s *sidecarStore) Get(desc Descriptor) (string, bool) {
	if s == nil || s.Dir == "" {
		return "", false
	}
	path := s.path(desc.Digest)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return "", false
	}
	// A sidecar is only ever renamed into place after its whole gzip stream
	// inflated cleanly, so its presence already means it is complete.
	//
	// Record the use: eviction orders by modification time, and without this a
	// sidecar that is read on every deployment would still look as old as the
	// pull that produced it and be reclaimed ahead of one never used again.
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	return path, true
}

// Plan picks the layers worth inflating during the pull, in descending size
// order, and stops once the filesystem has no room left to spare.
//
// The budget has to be tracked across the whole set rather than checked per
// layer: nothing is written yet while planning, so asking "does this one fit?"
// against the current free space would answer yes for every layer
// independently and collectively overrun the reserve.
func (s *sidecarStore) Plan(layers []Descriptor) []Descriptor {
	if s == nil || s.Dir == "" {
		return nil
	}
	free := availableAt(s.Dir)
	if free < 0 {
		return nil // cannot tell; do not gamble with the node's disk
	}
	budget := free - s.Reserve

	// Biggest first: a layer's inflate cost is what this hides, so when the
	// disk only affords some of them, the largest buy the most time.
	candidates := make([]Descriptor, 0, len(layers))
	for _, desc := range layers {
		if desc.Size < s.MinSize || strings.Contains(desc.MediaType, "zstd") {
			// Sidecars inflate gzip; zstd already decodes fast on the unpack path.
			continue
		}
		if _, ok := s.Get(desc); ok {
			continue // already inflated by an earlier pull
		}
		candidates = append(candidates, desc)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Size > candidates[j].Size })

	planned := make([]Descriptor, 0, len(candidates))
	for _, desc := range candidates {
		cost := desc.Size * inflatedSizeFactor
		if cost > budget {
			continue
		}
		budget -= cost
		planned = append(planned, desc)
	}
	return planned
}

// availableAt reports the free space for path, falling back to its nearest
// existing ancestor. The cache directory does not exist before a node's first
// pull, and statfs cannot answer for a path that is not there -- without this
// the very first pull, the one with nothing warm to reuse, would skip the
// optimization entirely.
func availableAt(path string) int64 {
	for {
		if free := availableBytes(path); free >= 0 {
			return free
		}
		parent := filepath.Dir(path)
		if parent == path {
			return -1
		}
		path = parent
	}
}

// Write inflates r into a sidecar for desc. The returned commit publishes the
// result when the blob it came from turns out to be valid, and discards it
// otherwise -- the bytes are read before the blob's digest has been checked.
func (s *sidecarStore) Write(ctx context.Context, desc Descriptor, r io.Reader) (func(bool), error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return nil, err
	}
	if s.Owner != nil {
		_ = os.Lchown(s.Dir, s.Owner.UID, s.Owner.GID)
	}
	tmp, err := os.CreateTemp(s.Dir, ".tar-*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	zr, err := kgzip.NewReader(r)
	if err != nil {
		cleanup()
		return nil, err
	}
	defer zr.Close()
	// io.Copy through the gzip reader validates the stream's CRC and length
	// trailer, so a clean return means the sidecar holds the layer in full.
	if _, err := io.Copy(tmp, zr); err != nil {
		cleanup()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		os.Remove(tmpName)
		return nil, err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return nil, err
	}
	if s.Owner != nil {
		_ = os.Lchown(tmpName, s.Owner.UID, s.Owner.GID)
	}
	return func(verified bool) {
		if !verified {
			os.Remove(tmpName)
			return
		}
		if err := os.Rename(tmpName, s.path(desc.Digest)); err != nil {
			os.Remove(tmpName)
		}
	}, nil
}

// sweep deletes partial sidecars left behind by an interrupted pull, and
// sidecars whose layer is no longer in layersDir.
//
// The second half bounds the cache. Layers are removed outside this package --
// by udocker itself, or by hand -- and a sidecar for a layer that no longer
// exists can never be used again, so without this it would occupy disk
// indefinitely. Tying the cache's lifetime to the layer store means its size
// stays proportional to the images actually held on the node.
func (s *sidecarStore) sweep(layersDir string) {
	if s == nil || s.Dir == "" {
		return
	}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".tar-") {
			_ = os.Remove(filepath.Join(s.Dir, name))
			continue
		}
		digest, ok := digestFromSidecar(name)
		if !ok {
			continue // not ours; leave it alone
		}
		if _, err := os.Stat(filepath.Join(layersDir, digest)); err != nil {
			_ = os.Remove(filepath.Join(s.Dir, name))
		}
	}
	s.evict()
}

// evict trims the cache to MaxTotal, discarding least-recently-used sidecars
// first.
//
// Without a ceiling the cache only ever grows: every new image adds its large
// layers and nothing is ever reclaimed, so a long-lived node would hand its
// disk to stale entries until the free-space guard quietly disabled the
// optimization for good. Bounding it keeps the cache useful indefinitely
// instead of useful once.
func (s *sidecarStore) evict() {
	if s.MaxTotal <= 0 {
		return
	}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return
	}
	type cached struct {
		path string
		size int64
		used time.Time
	}
	var held []cached
	var total int64
	for _, entry := range entries {
		if _, ok := digestFromSidecar(entry.Name()); !ok {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		held = append(held, cached{filepath.Join(s.Dir, entry.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	if total <= s.MaxTotal {
		return
	}
	sort.Slice(held, func(i, j int) bool { return held[i].used.Before(held[j].used) })
	for _, item := range held {
		if total <= s.MaxTotal {
			return
		}
		if os.Remove(item.path) == nil {
			total -= item.size
		}
	}
}

// digestFromSidecar reverses path's encoding, turning "sha256-abc.tar" back
// into "sha256:abc".
func digestFromSidecar(name string) (string, bool) {
	trimmed, ok := strings.CutSuffix(name, ".tar")
	if !ok {
		return "", false
	}
	algorithm, hex, ok := strings.Cut(trimmed, "-")
	if !ok || algorithm == "" || hex == "" {
		return "", false
	}
	return algorithm + ":" + hex, true
}
