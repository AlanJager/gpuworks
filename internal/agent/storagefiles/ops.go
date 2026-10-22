package storagefiles

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"gpuworks.ai/gpuworks/internal/proto"
)

// PathResult is the answer of mkdir, rename and delete: the path acted on.
type PathResult struct {
	Path string `json:"path"`
}

// List returns one page of a directory, read with lstat: links are reported,
// never followed. Region directories that do not exist yet are listed as
// empty directories so the writable regions are always reachable.
func (s *Service) List(_ context.Context, p proto.StorageFilesListParams) (proto.StorageFilesListResult, error) {
	root, err := s.root(p.StorageID, false)
	if err != nil {
		return proto.StorageFilesListResult{}, err
	}
	limit := p.Limit
	if limit <= 0 || limit > proto.StorageFilesListMaxLimit {
		limit = proto.StorageFilesListMaxLimit
	}
	var entries []proto.StorageFileEntry
	err = s.do(p.StorageID, "list", func() error {
		t, err := resolve(root, p.Path, true)
		if err != nil {
			return err
		}
		info, err := os.Stat(t.abs)
		if errors.Is(err, syscall.ENOTDIR) {
			err = fs.ErrNotExist
		}
		switch {
		case errors.Is(err, fs.ErrNotExist) && regionAncestor(t.rel):
			entries = virtualEntries(t.rel, nil)
			return nil
		case errors.Is(err, fs.ErrNotExist):
			return refuse(proto.StorageFilesErrNotFound, "%q does not exist", t.rel)
		case err != nil:
			return err
		case !info.IsDir():
			return refuse(proto.StorageFilesErrPathInvalid, "%q is not a directory", t.rel)
		}
		dirents, err := os.ReadDir(t.abs)
		if err != nil {
			return err
		}
		out := make([]proto.StorageFileEntry, 0, len(dirents))
		for _, d := range dirents {
			fi, err := d.Info() // lstat
			if err != nil {
				continue // gone since the directory was read
			}
			e := proto.StorageFileEntry{Name: d.Name(), Dir: fi.IsDir(), Symlink: fi.Mode()&fs.ModeSymlink != 0,
				ModTime: fi.ModTime().UTC()}
			if !e.Dir {
				e.Size = fi.Size()
			}
			out = append(out, e)
		}
		entries = virtualEntries(t.rel, out)
		return nil
	})
	if err != nil {
		return proto.StorageFilesListResult{}, err
	}
	return page(entries, p.Cursor, limit), nil
}

// virtualEntries adds the missing region directories below clean and sorts
// by name (byte order).
func virtualEntries(clean string, entries []proto.StorageFileEntry) []proto.StorageFileEntry {
	have := make(map[string]bool, len(entries))
	for _, e := range entries {
		have[e.Name] = true
	}
	for _, name := range virtualChildren(clean) {
		if !have[name] {
			have[name] = true
			entries = append(entries, proto.StorageFileEntry{Name: name, Dir: true})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	if entries == nil {
		entries = []proto.StorageFileEntry{}
	}
	return entries
}

func page(entries []proto.StorageFileEntry, cursor string, limit int) proto.StorageFilesListResult {
	start := 0
	if cursor != "" {
		start = sort.Search(len(entries), func(i int) bool { return entries[i].Name > cursor })
	}
	end := start + limit
	res := proto.StorageFilesListResult{}
	if end < len(entries) {
		res.Entries = entries[start:end]
		res.NextCursor = entries[end-1].Name
	} else {
		res.Entries = entries[start:]
	}
	return res
}

// writeTarget resolves a path a write acts on and checks it lies inside a
// writable region, both as named and after resolving links.
func writeTarget(root, rel string, followLast bool) (target, error) {
	clean, err := proto.CleanStorageRel(rel)
	if err != nil {
		return target{}, refuse(proto.StorageFilesErrPathInvalid, "%v", err)
	}
	if !proto.StorageRelWritable(clean) {
		return target{}, refuse(proto.StorageFilesErrRegionReadonly, "%q is outside the writable regions", clean)
	}
	t, err := resolve(root, clean, followLast)
	if err != nil {
		return target{}, err
	}
	if !t.writable() {
		return target{}, refuse(proto.StorageFilesErrRegionReadonly, "%q leads outside the writable regions (to %q)", clean, t.real)
	}
	return t, nil
}

// Mkdir creates a directory and its missing parents inside a writable
// region. An existing directory is not an error; an existing file is.
func (s *Service) Mkdir(_ context.Context, p proto.StorageFilesMkdirParams) (PathResult, error) {
	root, err := s.root(p.StorageID, true)
	if err != nil {
		return PathResult{}, err
	}
	var res PathResult
	err = s.do(p.StorageID, "mkdir", func() error {
		t, err := writeTarget(root, p.Path, true)
		if err != nil {
			return err
		}
		if info, err := os.Stat(t.abs); err == nil {
			if !info.IsDir() {
				return refuse(proto.StorageFilesErrExists, "%q exists and is not a directory", t.rel)
			}
		} else if err := os.MkdirAll(t.abs, 0o755); err != nil {
			return mkdirError(t.rel, err)
		}
		res = PathResult{Path: t.rel}
		return nil
	})
	return res, err
}

// mkdirError reports a parent that is a file as exists rather than a bare
// "not a directory".
func mkdirError(rel string, err error) error {
	if errors.Is(err, fs.ErrExist) || errors.Is(err, syscall.ENOTDIR) {
		return refuse(proto.StorageFilesErrExists, "a file is in the way of %q: %v", rel, err)
	}
	return err
}

// Rename moves a file or directory within the writable regions. The target
// must not exist.
func (s *Service) Rename(_ context.Context, p proto.StorageFilesRenameParams) (PathResult, error) {
	root, err := s.root(p.StorageID, true)
	if err != nil {
		return PathResult{}, err
	}
	var res PathResult
	err = s.do(p.StorageID, "rename", func() error {
		from, err := writeTarget(root, p.From, false)
		if err != nil {
			return err
		}
		to, err := writeTarget(root, p.To, false)
		if err != nil {
			return err
		}
		if to.rel == from.rel || strings.HasPrefix(to.rel, from.rel+"/") || strings.HasPrefix(to.real, from.real+"/") {
			return refuse(proto.StorageFilesErrPathInvalid, "cannot move %q into itself", from.rel)
		}
		if _, err := os.Lstat(from.abs); errors.Is(err, fs.ErrNotExist) {
			return refuse(proto.StorageFilesErrNotFound, "%q does not exist", from.rel)
		} else if err != nil {
			return err
		}
		if _, err := os.Lstat(to.abs); err == nil {
			return refuse(proto.StorageFilesErrExists, "%q already exists", to.rel)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return mkdirError(to.rel, err)
		}
		if err := os.MkdirAll(filepath.Dir(to.abs), 0o755); err != nil {
			return mkdirError(to.rel, err)
		}
		if err := os.Rename(from.abs, to.abs); err != nil {
			return err
		}
		res = PathResult{Path: to.rel}
		return nil
	})
	return res, err
}

// Delete moves a file or directory inside a writable region into the
// storage's trash and deletes its bytes in the background.
func (s *Service) Delete(_ context.Context, p proto.StorageFilesDeleteParams) (PathResult, error) {
	root, err := s.root(p.StorageID, true)
	if err != nil {
		return PathResult{}, err
	}
	var res PathResult
	var trash string
	err = s.do(p.StorageID, "delete", func() error {
		t, err := writeTarget(root, p.Path, false)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(t.abs); errors.Is(err, fs.ErrNotExist) {
			return refuse(proto.StorageFilesErrNotFound, "%q does not exist", t.rel)
		} else if err != nil {
			return err
		}
		dst, err := s.trashFor(root, t.abs)
		if err != nil {
			return err
		}
		if err := os.Rename(t.abs, dst); err != nil {
			return err
		}
		res, trash = PathResult{Path: t.rel}, dst
		return nil
	})
	if err != nil {
		return PathResult{}, err
	}
	s.removeInBackground(trash)
	return res, nil
}
