package modelmetadata

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path"
	"strings"
)

const maxDocumentBytes = 2 << 20

func readDocument(ctx context.Context, files FileView, name string) ([]byte, error) {
	info, err := files.Stat(ctx, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.IsDir {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrInvalid}
	}
	if info.Size > maxDocumentBytes {
		return nil, ErrReadLimit
	}
	return files.ReadRange(ctx, name, 0, info.Size)
}

func readJSON(ctx context.Context, files FileView, name string, target any) (bool, error) {
	body, err := readDocument(ctx, files, name)
	if err != nil || body == nil {
		return false, err
	}
	if err := json.Unmarshal(body, target); err != nil {
		return true, err
	}
	return true, nil
}

func childPath(base, name string) (string, error) {
	if name == "" || path.IsAbs(name) || strings.Contains(name, "\\") {
		return "", ErrOutsideScope
	}
	p := path.Clean(path.Join(base, name))
	if !fs.ValidPath(p) {
		return "", ErrOutsideScope
	}
	return p, nil
}

func exists(ctx context.Context, files FileView, name string) bool {
	i, err := files.Stat(ctx, name)
	return err == nil && !i.IsDir && i.Size > 0
}

func hasName(items []FileInfo, name string) bool {
	for _, item := range items {
		if item.Name == name {
			return true
		}
	}
	return false
}

func issue(name, code string, err error) Issue {
	if errors.Is(err, ErrReadLimit) {
		code = "metadata_read_limit"
	}
	i := Issue{Path: name, Code: code}
	if err != nil {
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			i.Path = pathErr.Path
		}
		i.Detail = err.Error()
	}
	return i
}

type rangeKey struct {
	path           string
	offset, length int64
}
type cachedView struct {
	FileView
	dirs      map[string][]FileInfo
	stats     map[string]FileInfo
	reads     map[rangeKey][]byte
	readBytes int64
}

func cacheView(view FileView) *cachedView {
	return &cachedView{FileView: view, dirs: map[string][]FileInfo{}, stats: map[string]FileInfo{}, reads: map[rangeKey][]byte{}}
}
func (v *cachedView) ReadDir(ctx context.Context, name string) ([]FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if found, ok := v.dirs[name]; ok {
		return found, nil
	}
	found, err := v.FileView.ReadDir(ctx, name)
	if err == nil {
		v.dirs[name] = found
	}
	return found, err
}
func (v *cachedView) Stat(ctx context.Context, name string) (FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return FileInfo{}, err
	}
	if found, ok := v.stats[name]; ok {
		return found, nil
	}
	found, err := v.FileView.Stat(ctx, name)
	if err == nil {
		v.stats[name] = found
	}
	return found, err
}
func (v *cachedView) ReadRange(ctx context.Context, name string, offset, length int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := rangeKey{name, offset, length}
	if found, ok := v.reads[key]; ok {
		return found, nil
	}
	found, err := v.FileView.ReadRange(ctx, name, offset, length)
	// Streaming indices and GGUF token tables must not accumulate their
	// entire contents in the cache. Small shared documents still reuse reads.
	if err == nil && v.readBytes+int64(len(found)) <= maxDocumentBytes {
		v.reads[key] = found
		v.readBytes += int64(len(found))
	}
	return found, err
}
