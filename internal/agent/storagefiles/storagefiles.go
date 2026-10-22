// Package storagefiles is the agent side of storage files (ADR 0053): it lists
// a storage's tree, creates, renames and deletes inside the writable regions,
// receives uploads (optionally unpacking an archive) and sends files or packed
// directories back. Every path is resolved against the storage root here,
// after symbolic links, and refused when it would leave the root or, for a
// write, a writable region; the server's checks are a preflight, not the
// guard.
//
// The package knows nothing about how a storage is mounted: the caller tells
// it where a storage's root is and whether it is usable now (Options.Root),
// and how to bound a call into a share that may hang (Options.Do).
package storagefiles

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/storagemount"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Root is a storage as this node sees it now.
type Root struct {
	// Dir is the storage root: a local storage's directory, a shared
	// storage's mount point (for nfs_cache the authoritative NFS mount, never
	// the node cache).
	Dir string
	// Ready: the storage is usable on this node. Writable: this node may
	// write to it.
	Ready, Writable bool
}

// Options wires a Service to the node.
type Options struct {
	// Root resolves a storage id. An error means the storage is not
	// configured on this node.
	Root func(storageID uint) (Root, error)
	// Do runs one short filesystem call under a storage with a time limit
	// (see storagemount.Manager.DoAny). nil runs fn directly.
	Do func(storageID uint, op string, timeout time.Duration, fn func() error) error
	// Available is the free space of the filesystem holding a path; it
	// bounds what an archive may unpack to. nil means no space bound (the
	// entry bound still applies).
	Available func(path string) (int64, error)
	// Now defaults to time.Now.
	Now func() time.Time
	Log *slog.Logger
	// OpTimeout bounds one metadata call; default 30s.
	OpTimeout time.Duration
	// ExtractReserve is the free space an extraction leaves untouched;
	// default 1 GiB. MaxExtractEntries bounds the entries of one archive;
	// default 1,000,000.
	ExtractReserve    int64
	MaxExtractEntries int
}

// Defaults of the extraction limits (contract: free space − 1 GiB, at most a
// million entries).
const (
	DefaultExtractReserve    = int64(1) << 30
	DefaultMaxExtractEntries = 1_000_000
	defaultOpTimeout         = 30 * time.Second
)

// Service serves storage files for one node. It is safe for concurrent use.
type Service struct{ o Options }

// New returns a Service with defaults filled in.
func New(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.OpTimeout <= 0 {
		o.OpTimeout = defaultOpTimeout
	}
	if o.ExtractReserve <= 0 {
		o.ExtractReserve = DefaultExtractReserve
	}
	if o.MaxExtractEntries <= 0 {
		o.MaxExtractEntries = DefaultMaxExtractEntries
	}
	return &Service{o: o}
}

func refuse(code, format string, args ...any) error {
	return proto.NewStorageFilesError(code, format, args...)
}

// asFilesError gives every error leaving the package a storage-files code:
// a refusal keeps its own, a missing path is not_found, anything else is
// failed with the error text as detail.
func asFilesError(err error) *proto.StorageFilesError {
	if err == nil {
		return nil
	}
	var fe *proto.StorageFilesError
	if errors.As(err, &fe) {
		return fe
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return proto.NewStorageFilesError(proto.StorageFilesErrNotFound, "%v", err)
	case errors.Is(err, fs.ErrExist):
		return proto.NewStorageFilesError(proto.StorageFilesErrExists, "%v", err)
	}
	return proto.NewStorageFilesError(proto.StorageFilesErrFailed, "%v", err)
}

// root returns the storage's root when it is usable for the operation.
func (s *Service) root(storageID uint, write bool) (string, error) {
	if s.o.Root == nil {
		return "", refuse(proto.StorageFilesErrNotWritable, "storages are not available on this agent")
	}
	r, err := s.o.Root(storageID)
	if err != nil {
		return "", refuse(proto.StorageFilesErrNotWritable, "%v", err)
	}
	if !r.Ready || strings.TrimSpace(r.Dir) == "" {
		return "", refuse(proto.StorageFilesErrNotWritable, "storage %d is not ready on this node", storageID)
	}
	if write && !r.Writable {
		return "", refuse(proto.StorageFilesErrNotWritable, "storage %d is read-only on this node", storageID)
	}
	return filepath.Clean(r.Dir), nil
}

// do runs fn under the storage's guard; the error that comes back always
// carries a storage-files code.
func (s *Service) do(storageID uint, op string, fn func() error) error {
	var err error
	if s.o.Do == nil {
		err = fn()
	} else {
		err = s.o.Do(storageID, "files."+op, s.o.OpTimeout, fn)
	}
	if err == nil {
		return nil
	}
	return asFilesError(err)
}

// target is a storage path resolved on this node.
type target struct {
	// rel is the clean path as the client named it ("" = the root); abs is
	// the same below the root, without resolving links.
	rel, abs string
	// real is rel after the symbolic links of its existing part were
	// resolved, relative to the resolved root. It is what a region check of
	// a write must pass too: a link inside files/ that points at a model
	// directory must not make the model writable.
	real string
}

// writable reports whether both the named and the resolved path lie inside a
// writable region (strictly: not a region directory itself).
func (t target) writable() bool {
	return proto.StorageRelWritable(t.rel) && proto.StorageRelWritable(t.real)
}

// resolve turns a client path into a target below root. With followLast the
// last component is resolved like the others (the operation acts on what the
// path points to: list, download, mkdir); without it the last component is
// taken as is (the operation acts on the entry: delete, rename, the target of
// an upload). The deepest existing ancestor is resolved with EvalSymlinks and
// must stay inside the resolved root, otherwise the path is path_invalid.
func resolve(root, rel string, followLast bool) (target, error) {
	clean, err := proto.CleanStorageRel(rel)
	if err != nil {
		return target{}, refuse(proto.StorageFilesErrPathInvalid, "%v", err)
	}
	root = filepath.Clean(root)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return target{}, refuse(proto.StorageFilesErrNotWritable, "storage root: %v", err)
	}
	if clean == "" {
		return target{rel: "", abs: root, real: ""}, nil
	}
	abs, err := proto.JoinWithin(root, filepath.FromSlash(clean))
	if err != nil {
		return target{}, refuse(proto.StorageFilesErrPathInvalid, "%v", err)
	}
	probe, tail := abs, ""
	if !followLast {
		probe, tail = filepath.Dir(abs), filepath.Base(abs)
	}
	existing := probe
	var rest []string
	for existing != root {
		_, err := os.Lstat(existing)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return target{}, asFilesError(err)
		}
		rest = append([]string{filepath.Base(existing)}, rest...)
		existing = filepath.Dir(existing)
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		// A dangling link or a loop: what it points to cannot be checked,
		// and creating below it would follow it wherever it leads.
		return target{}, refuse(proto.StorageFilesErrPathInvalid, "path %q: %v", clean, err)
	}
	within, err := filepath.Rel(realRoot, resolved)
	if err != nil || filepath.IsAbs(within) || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return target{}, refuse(proto.StorageFilesErrPathInvalid, "path %q resolves outside the storage", clean)
	}
	parts := []string{filepath.ToSlash(within)}
	parts = append(parts, rest...)
	if tail != "" {
		parts = append(parts, tail)
	}
	real := strings.TrimPrefix(path.Clean("/"+path.Join(parts...)), "/")
	return target{rel: clean, abs: abs, real: real}, nil
}

// regionAncestor reports whether a clean path is a region directory or one of
// its ancestors (the root, .gpuworks): such a directory is listed even before
// it exists, so the writable regions are always reachable.
func regionAncestor(clean string) bool {
	for _, region := range proto.StorageWritableRegions() {
		if clean == "" || clean == region || strings.HasPrefix(region, clean+"/") {
			return true
		}
	}
	return false
}

// virtualChildren are the names a listing of clean shows for region
// directories (or their ancestors) that do not exist yet.
func virtualChildren(clean string) []string {
	var out []string
	for _, region := range proto.StorageWritableRegions() {
		var rest string
		switch {
		case clean == "":
			rest = region
		case strings.HasPrefix(region, clean+"/"):
			rest = strings.TrimPrefix(region, clean+"/")
		default:
			continue
		}
		name, _, _ := strings.Cut(rest, "/")
		out = append(out, name)
	}
	return out
}

// trashFor is where an entry removed from root goes, checked to lie below the
// root's own .gpuworks/trash with no link on the way: the bytes of a delete
// must not land outside the storage.
func (s *Service) trashFor(root, name string) (string, error) {
	dir := path.Join(proto.StorageMetaDir, proto.StorageTrashDir)
	t, err := resolve(root, dir+"/x", false)
	if err != nil {
		return "", err
	}
	if t.real != t.rel {
		return "", refuse(proto.StorageFilesErrPathInvalid, "the storage's trash directory is a link")
	}
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o755); err != nil {
		return "", err
	}
	return storagemount.TrashPath(root, name, s.o.Now()), nil
}

// removeInBackground deletes what was moved into the trash; a failure leaves
// it for the trash sweep.
func (s *Service) removeInBackground(p string) {
	go func() {
		if err := os.RemoveAll(p); err != nil {
			s.o.Log.Warn("removing a deleted storage file failed", "path", p, "err", err)
		}
	}()
}

// ctxReader stops a copy when its context ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
