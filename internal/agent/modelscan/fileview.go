package modelscan

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"sort"

	"gpuworks.ai/gpuworks/pkg/model/modelmetadata"
)

// LocalView confines every operation, including symlink resolution, to one
// confirmed repository (or the candidate itself if its boundary is unknown).
type LocalView struct {
	root      *os.Root
	readBytes int64
}

// Per-view I/O guard; format readers impose smaller per-document limits.
// Large sharded checkpoints can have tens of MiB of index metadata, read in
// chunks without retaining the tensor-to-shard map or loading weights.
const maxViewReadBytes = 256 << 20

func OpenView(dir string) (*LocalView, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &LocalView{root: root}, nil
}
func (v *LocalView) Close() error { return v.root.Close() }
func viewPath(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !fs.ValidPath(name) {
		return modelmetadata.ErrOutsideScope
	}
	return nil
}
func (v *LocalView) Stat(ctx context.Context, name string) (modelmetadata.FileInfo, error) {
	if err := viewPath(ctx, name); err != nil {
		return modelmetadata.FileInfo{}, err
	}
	i, err := v.root.Stat(name)
	if err != nil {
		return modelmetadata.FileInfo{}, err
	}
	if !i.IsDir() && !i.Mode().IsRegular() {
		return modelmetadata.FileInfo{}, fs.ErrInvalid
	}
	return modelmetadata.FileInfo{Name: i.Name(), IsDir: i.IsDir(), Size: i.Size()}, nil
}
func (v *LocalView) ReadDir(ctx context.Context, name string) ([]modelmetadata.FileInfo, error) {
	if err := viewPath(ctx, name); err != nil {
		return nil, err
	}
	f, err := v.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(100001)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > 100000 {
		return nil, modelmetadata.ErrReadLimit
	}
	out := make([]modelmetadata.FileInfo, 0, len(entries))
	for _, e := range entries {
		// List names without following a link; any subsequent read or stat is
		// still confined by os.Root. This preserves evidence of broken shards.
		i, err := e.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, modelmetadata.FileInfo{Name: e.Name(), IsDir: e.IsDir(), Size: i.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
func (v *LocalView) ReadRange(ctx context.Context, name string, offset, length int64) ([]byte, error) {
	if err := viewPath(ctx, name); err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 || length > 2<<20 || v.readBytes+length > maxViewReadBytes {
		return nil, modelmetadata.ErrReadLimit
	}
	info, err := v.Stat(ctx, name)
	if err != nil {
		return nil, err
	}
	if info.IsDir {
		return nil, fs.ErrInvalid
	}
	f, err := v.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	v.readBytes += length
	b := make([]byte, int(length))
	n, err := f.ReadAt(b, offset)
	return b[:n], err
}
