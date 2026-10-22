package storagefiles

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// body is the upload's byte stream: it counts what arrives, remembers how the
// stream ended, and refuses more bytes than were announced.
type body struct {
	r     io.Reader
	limit int64 // announced size, -1 unknown
	n     int64
	eof   bool
	err   error // a read error other than EOF: the stream broke
}

func (b *body) Read(p []byte) (int, error) {
	if b.limit >= 0 {
		room := b.limit - b.n + 1
		if room <= 0 {
			return 0, refuse(proto.StorageFilesErrFailed, "received more than the announced %d bytes", b.limit)
		}
		if int64(len(p)) > room {
			p = p[:room]
		}
	}
	n, err := b.r.Read(p)
	b.n += int64(n)
	switch {
	case err == io.EOF:
		b.eof = true
	case err != nil:
		b.err = err
	}
	if b.limit >= 0 && b.n > b.limit {
		return n, refuse(proto.StorageFilesErrFailed, "received more than the announced %d bytes", b.limit)
	}
	return n, err
}

// Upload receives one StreamStorageUpload: the bytes go to the staging
// directory (unpacked there when the header asks for it), and only once the
// stream has ended and the result has been renamed into place does the agent
// answer with one StorageFilesStatus line. On a failure the staging directory
// is removed and the target is left as it was. A refusal may be answered
// before the body has been read; the agent then keeps reading (and dropping)
// the body to its end, so a server that reads the answer only after sending
// everything still sees it.
func (s *Service) Upload(ctx context.Context, hdr proto.StreamHeader, rw io.ReadWriter) error {
	limit := hdr.Size
	if limit < 0 {
		limit = -1
	}
	b := &body{r: rw, limit: limit}
	files, err := s.receive(ctx, hdr, b)
	status := proto.StorageFilesStatus{State: proto.StorageFilesDone, Bytes: b.n, Files: files}
	if err != nil {
		fe := asFilesError(err)
		status = proto.StorageFilesStatus{State: proto.StorageFilesFailed, Code: fe.Code, Error: fe.Error(), Bytes: b.n}
		s.o.Log.Warn("storage upload failed", "storage", hdr.StorageID, "path", hdr.Rel, "bytes", b.n, "err", fe)
	}
	line, _ := json.Marshal(status)
	_, writeErr := rw.Write(append(line, '\n'))
	if err != nil {
		if !b.eof && b.err == nil && ctx.Err() == nil {
			b.limit = -1
			_, _ = io.Copy(io.Discard, ctxReader{ctx: ctx, r: b})
		}
		return err
	}
	return writeErr
}

// stagingValid checks a staging path has the one shape uploads use:
// .gpuworks/incoming/<control plane>/upload-<id>.
func stagingValid(clean string) bool {
	parts := strings.Split(clean, "/")
	return len(parts) == 4 && parts[0] == proto.StorageMetaDir && parts[1] == proto.StorageIncomingDir &&
		parts[2] != "" && strings.HasPrefix(parts[3], "upload-") && len(parts[3]) > len("upload-")
}

func (s *Service) receive(ctx context.Context, hdr proto.StreamHeader, b *body) (int, error) {
	if hdr.Size < -1 {
		return 0, refuse(proto.StorageFilesErrFailed, "invalid upload size %d", hdr.Size)
	}
	if hdr.Extract != "" && !proto.StorageArchiveFormatValid(hdr.Extract) {
		return 0, refuse(proto.StorageFilesErrFailed, "unknown archive format %q", hdr.Extract)
	}
	rel, err := proto.CleanStorageRel(hdr.Rel)
	if err != nil {
		return 0, refuse(proto.StorageFilesErrPathInvalid, "%v", err)
	}
	if !proto.StorageRelWritable(rel) {
		return 0, refuse(proto.StorageFilesErrRegionReadonly, "%q is outside the writable regions", rel)
	}
	staging, err := proto.CleanStorageRel(hdr.Staging)
	if err != nil || !stagingValid(staging) {
		return 0, refuse(proto.StorageFilesErrPathInvalid, "invalid staging path %q", hdr.Staging)
	}
	root, err := s.root(hdr.StorageID, true)
	if err != nil {
		return 0, err
	}
	var stage string
	err = s.do(hdr.StorageID, "upload", func() error {
		t, err := writeTarget(root, rel, false)
		if err != nil {
			return err
		}
		if !hdr.Overwrite {
			if _, err := os.Lstat(t.abs); err == nil {
				return refuse(proto.StorageFilesErrExists, "%q already exists", t.rel)
			}
		}
		st, err := resolve(root, staging, false)
		if err != nil {
			return err
		}
		if st.real != st.rel {
			return refuse(proto.StorageFilesErrPathInvalid, "the staging path %q goes through a link", staging)
		}
		if err := os.MkdirAll(filepath.Dir(st.abs), 0o755); err != nil {
			return err
		}
		if err := os.Mkdir(st.abs, 0o700); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return refuse(proto.StorageFilesErrFailed, "staging path %q is in use", staging)
			}
			return err
		}
		stage = st.abs
		return nil
	})
	if err != nil {
		return 0, err
	}
	defer func() {
		if err := os.RemoveAll(stage); err != nil {
			s.o.Log.Warn("removing an upload's staging directory failed", "path", stage, "err", err)
		}
	}()

	result, files, err := s.receiveBody(ctx, hdr.Extract, stage, b)
	if err != nil {
		return 0, s.classify(ctx, b, hdr.Extract, err)
	}
	if hdr.Size >= 0 && b.n != hdr.Size {
		return 0, refuse(proto.StorageFilesErrFailed, "received %d bytes, %d were announced", b.n, hdr.Size)
	}
	if err := s.commit(hdr, root, rel, result); err != nil {
		return 0, err
	}
	return files, nil
}

// classify names what broke: the stream, the context, the storage (already
// a refusal) or else the archive.
func (s *Service) classify(ctx context.Context, b *body, format string, err error) error {
	var fe *proto.StorageFilesError
	switch {
	case errors.As(err, &fe):
		return err
	case ctx.Err() != nil:
		return refuse(proto.StorageFilesErrFailed, "upload cancelled: %v", ctx.Err())
	case b.err != nil:
		return refuse(proto.StorageFilesErrFailed, "upload stream broke: %v", b.err)
	case format == "":
		return refuse(proto.StorageFilesErrFailed, "%v", err)
	default:
		return describeExtract(format, err)
	}
}

// receiveBody reads the whole stream into the staging directory and returns
// the path to rename into place.
func (s *Service) receiveBody(ctx context.Context, format, stage string, b *body) (string, int, error) {
	src := ctxReader{ctx: ctx, r: b}
	if format == "" {
		p := filepath.Join(stage, "body")
		if err := copyToFile(p, src); err != nil {
			return "", 0, err
		}
		return p, 1, nil
	}
	out := filepath.Join(stage, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		return "", 0, err
	}
	e := &extractor{dir: out, maxEntries: s.o.MaxExtractEntries}
	switch format {
	case proto.StorageArchiveZip:
		archive := filepath.Join(stage, "body.zip")
		if err := copyToFile(archive, src); err != nil {
			return "", 0, err
		}
		e.maxBytes = s.extractBound(stage)
		if err := e.extractZip(ctx, archive); err != nil {
			return "", 0, err
		}
		_ = os.Remove(archive)
	case proto.StorageArchiveTar:
		e.maxBytes = s.extractBound(stage)
		if err := e.extractTar(ctx, src); err != nil {
			return "", 0, err
		}
	default: // tar.gz
		e.maxBytes = s.extractBound(stage)
		gz, err := gzip.NewReader(src)
		if err != nil {
			return "", 0, err
		}
		if err := e.extractTar(ctx, gz); err != nil {
			return "", 0, err
		}
		// The rest of the gzip stream carries its checksum.
		if _, err := io.Copy(io.Discard, gz); err != nil {
			return "", 0, err
		}
	}
	// Whatever follows the archive's end (tar padding) is part of the upload:
	// only the stream's own end proves everything arrived.
	if _, err := io.Copy(io.Discard, src); err != nil {
		return "", 0, err
	}
	return out, e.files, nil
}

func copyToFile(p string, r io.Reader) error {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(fsWriter{f}, r)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return fsFailed(closeErr)
	}
	return nil
}

// extractBound is how much an archive may unpack to: the free space of the
// staging filesystem less the reserve, or no bound without a statfs.
func (s *Service) extractBound(stage string) int64 {
	if s.o.Available == nil {
		return -1
	}
	avail, err := s.o.Available(stage)
	if err != nil {
		s.o.Log.Warn("free space of a storage unknown; extraction unbounded by size", "path", stage, "err", err)
		return -1
	}
	if bound := avail - s.o.ExtractReserve; bound > 0 {
		return bound
	}
	return 0
}

// commit renames the staged result onto the target. With Overwrite an
// existing target goes to the trash first and comes back if the rename fails.
func (s *Service) commit(hdr proto.StreamHeader, root, rel, result string) error {
	var old string
	err := s.do(hdr.StorageID, "upload.commit", func() error {
		t, err := writeTarget(root, rel, false)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(t.abs); err == nil {
			if !hdr.Overwrite {
				return refuse(proto.StorageFilesErrExists, "%q already exists", t.rel)
			}
			dst, err := s.trashFor(root, t.abs)
			if err != nil {
				return err
			}
			if err := os.Rename(t.abs, dst); err != nil {
				return err
			}
			old = dst
		} else if !errors.Is(err, fs.ErrNotExist) {
			return mkdirError(t.rel, err)
		}
		if err := os.MkdirAll(filepath.Dir(t.abs), 0o755); err != nil {
			return mkdirError(t.rel, err)
		}
		if err := os.Rename(result, t.abs); err != nil {
			if old != "" {
				if back := os.Rename(old, t.abs); back == nil {
					old = ""
				}
			}
			return err
		}
		return nil
	})
	if err == nil && old != "" {
		s.removeInBackground(old)
	}
	return err
}
