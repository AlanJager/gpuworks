package packages

import (
	"context"
	"io/fs"
)

// cancellableFS checks cancellation around ordinary fs operations. A single
// blocked filesystem call still relies on the operating system to return.
type cancellableFS struct {
	fs.FS
	ctx context.Context
}

func (c cancellableFS) Open(name string) (fs.File, error) {
	if err := c.ctx.Err(); err != nil {
		return nil, err
	}
	file, err := c.FS.Open(name)
	if err != nil {
		return nil, err
	}
	if err := c.ctx.Err(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return cancellableFile{File: file, ctx: c.ctx}, nil
}

func (c cancellableFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if err := c.ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(c.FS, name)
	if cancelErr := c.ctx.Err(); cancelErr != nil {
		return nil, cancelErr
	}
	return entries, err
}

type cancellableFile struct {
	fs.File
	ctx context.Context
}

func (f cancellableFile) Read(p []byte) (int, error) {
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := f.File.Read(p)
	if cancelErr := f.ctx.Err(); cancelErr != nil {
		return n, cancelErr
	}
	return n, err
}
