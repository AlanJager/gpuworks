package storagefiles

import (
	"archive/tar"
	"archive/zip"
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// SkippedLinksFile lists, inside a directory download, the links that were
// left out (one path per line, relative to the downloaded directory).
const SkippedLinksFile = "SKIPPED_SYMLINKS.txt"

// Download answers one StreamStorageDownload: a StorageDownloadHead line,
// then, unless the head carries a refusal, the file as is or the directory
// packed in the requested format. Links are never followed: a link named
// directly is refused, and one met inside a directory is left out and listed
// in SkippedLinksFile. A failure after the head returns an error and the
// caller closes the stream early, which the server reads as a truncated
// transfer.
func (s *Service) Download(ctx context.Context, hdr proto.StreamHeader, w io.Writer) error {
	src, err := s.openDownload(hdr)
	if err != nil {
		fe := asFilesError(err)
		_ = writeHead(w, proto.StorageDownloadHead{Code: fe.Code, Error: fe.Error()})
		return fe
	}
	if src.file != nil {
		defer src.file.Close()
	}
	if err := writeHead(w, src.head); err != nil {
		return err
	}
	if !src.head.Dir {
		_, err := io.CopyN(w, ctxReader{ctx: ctx, r: src.file}, src.head.Size)
		return err
	}
	return packDir(ctx, src.abs, src.head.Name, src.format, w)
}

type downloadSource struct {
	head   proto.StorageDownloadHead
	abs    string
	format string
	file   *os.File
}

func writeHead(w io.Writer, head proto.StorageDownloadHead) error {
	line, _ := json.Marshal(head)
	_, err := w.Write(append(line, '\n'))
	return err
}

func (s *Service) openDownload(hdr proto.StreamHeader) (downloadSource, error) {
	format := hdr.Format
	if format == "" {
		format = proto.StorageArchiveZip
	}
	if !proto.StorageArchiveFormatValid(format) {
		return downloadSource{}, refuse(proto.StorageFilesErrFailed, "unknown archive format %q", hdr.Format)
	}
	root, err := s.root(hdr.StorageID, false)
	if err != nil {
		return downloadSource{}, err
	}
	var src downloadSource
	err = s.do(hdr.StorageID, "download", func() error {
		t, err := resolve(root, hdr.Rel, true)
		if err != nil {
			return err
		}
		info, err := os.Lstat(t.abs)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return refuse(proto.StorageFilesErrNotFound, "%q does not exist", t.rel)
		} else if err != nil {
			return err
		}
		name := path.Base(t.rel)
		if t.rel == "" {
			name = "storage"
		}
		switch {
		case t.rel != "" && info.Mode()&fs.ModeSymlink != 0:
			return refuse(proto.StorageFilesErrPathInvalid, "%q is a symbolic link; links are not followed", t.rel)
		case info.IsDir():
			src = downloadSource{head: proto.StorageDownloadHead{Dir: true, Size: -1, Name: name}, abs: t.abs, format: format}
			return nil
		case !info.Mode().IsRegular():
			return refuse(proto.StorageFilesErrPathInvalid, "%q is not a regular file or directory", t.rel)
		}
		f, err := os.Open(t.abs)
		if err != nil {
			return err
		}
		src = downloadSource{head: proto.StorageDownloadHead{Size: info.Size(), Name: name}, abs: t.abs, file: f}
		return nil
	})
	return src, err
}

// packer writes one archive.
type packer interface {
	dir(name string, info fs.FileInfo) error
	file(name string, info fs.FileInfo, r io.Reader) error
	close() error
}

func newPacker(format string, w io.Writer) packer {
	if format == proto.StorageArchiveZip {
		zw := zip.NewWriter(w)
		// Model weights barely compress: the fastest level keeps a large
		// directory from being CPU bound on the agent.
		zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
			return flate.NewWriter(out, flate.BestSpeed)
		})
		return &zipPacker{zw: zw}
	}
	p := &tarPacker{}
	if format == proto.StorageArchiveTarGz {
		gz, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
		p.gz = gz
		w = gz
	}
	p.tw = tar.NewWriter(w)
	return p
}

// packDir walks dir without following links and packs it below name/.
func packDir(ctx context.Context, dir, name, format string, w io.Writer) error {
	p := newPacker(format, w)
	var skipped []string
	top := map[string]bool{}
	walkErr := filepath.WalkDir(dir, func(p2 string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p2)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		entry := name
		if rel != "." {
			entry = name + "/" + rel
			if !strings.Contains(rel, "/") {
				top[rel] = true
			}
		}
		info, err := d.Info() // lstat
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			skipped = append(skipped, rel)
			return nil
		case info.IsDir():
			return p.dir(entry, info)
		case !info.Mode().IsRegular():
			skipped = append(skipped, rel+" (not a regular file)")
			return nil
		}
		f, err := os.Open(p2)
		if err != nil {
			return err
		}
		defer f.Close()
		return p.file(entry, info, ctxReader{ctx: ctx, r: f})
	})
	if walkErr != nil {
		_ = p.close()
		return walkErr
	}
	if len(skipped) > 0 {
		listName := SkippedLinksFile
		for i := 1; top[listName]; i++ {
			listName = fmt.Sprintf("SKIPPED_SYMLINKS-%d.txt", i)
		}
		body := strings.Join(skipped, "\n") + "\n"
		if err := p.file(name+"/"+listName, listInfo{size: int64(len(body)), mod: time.Now()}, strings.NewReader(body)); err != nil {
			_ = p.close()
			return err
		}
	}
	return p.close()
}

type zipPacker struct{ zw *zip.Writer }

func (z *zipPacker) dir(name string, info fs.FileInfo) error {
	_, err := z.zw.CreateHeader(&zip.FileHeader{Name: name + "/", Method: zip.Store, Modified: info.ModTime()})
	return err
}

func (z *zipPacker) file(name string, info fs.FileInfo, r io.Reader) error {
	fh, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	fh.Name = name
	fh.Method = zip.Deflate
	fw, err := z.zw.CreateHeader(fh)
	if err != nil {
		return err
	}
	_, err = io.CopyN(fw, r, info.Size())
	return err
}

func (z *zipPacker) close() error { return z.zw.Close() }

type tarPacker struct {
	gz *gzip.Writer
	tw *tar.Writer
}

func (t *tarPacker) header(name string, info fs.FileInfo) (*tar.Header, error) {
	h, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return nil, err
	}
	h.Name, h.Uname, h.Gname = name, "", ""
	return h, nil
}

func (t *tarPacker) dir(name string, info fs.FileInfo) error {
	h, err := t.header(name+"/", info)
	if err != nil {
		return err
	}
	return t.tw.WriteHeader(h)
}

func (t *tarPacker) file(name string, info fs.FileInfo, r io.Reader) error {
	h, err := t.header(name, info)
	if err != nil {
		return err
	}
	if err := t.tw.WriteHeader(h); err != nil {
		return err
	}
	_, err = io.CopyN(t.tw, r, info.Size())
	return err
}

func (t *tarPacker) close() error {
	err := t.tw.Close()
	if t.gz != nil {
		if gzErr := t.gz.Close(); err == nil {
			err = gzErr
		}
	}
	return err
}

// listInfo describes the generated SkippedLinksFile.
type listInfo struct {
	size int64
	mod  time.Time
}

func (l listInfo) Name() string       { return SkippedLinksFile }
func (l listInfo) Size() int64        { return l.size }
func (l listInfo) Mode() fs.FileMode  { return 0o644 }
func (l listInfo) ModTime() time.Time { return l.mod }
func (l listInfo) IsDir() bool        { return false }
func (l listInfo) Sys() any           { return nil }
