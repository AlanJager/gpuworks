package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Storage files streams (ADR 0053): an upload carries one request body to the
// agent and waits for its one-line verdict; a download reads the agent's head
// line and hands the bytes after it to the caller. Both ride the data lane and
// need it attached now: a transfer of arbitrary size never waits for a lane to
// come back, and never falls back to ctrl.

// storageDownloadHeadTimeout bounds the wait for a download's head line; the
// agent writes it after resolving the path, before any byte of the file.
const storageDownloadHeadTimeout = time.Minute

// storageUploadRefusalWait is how long a broken upload write waits for the
// agent's refusal line, which explains the break better than the write error.
const storageUploadRefusalWait = 2 * time.Second

// storageHeadLineMax bounds a head or status line; both are a few hundred
// bytes of JSON.
const storageHeadLineMax = 64 << 10

// DataLaneAttached reports whether a node's data lane is attached now: the
// condition a storage-files stream needs to open.
func (h *Hub) DataLaneAttached(nodeUUID string) bool {
	s := h.session(nodeUUID)
	return s != nil && s.DataLaneState() == "attached"
}

func (h *Hub) openStorageStream(ctx context.Context, nodeUUID string, hdr proto.StreamHeader) (net.Conn, error) {
	s := h.session(nodeUUID)
	if s == nil {
		return nil, errors.New("node not connected: " + nodeUUID)
	}
	if s.DataLaneState() != "attached" {
		return nil, fmt.Errorf("%w: node %s has no data lane for a storage transfer", ErrDataLaneUnavailable, nodeUUID)
	}
	return s.openStream(ctx, hdr)
}

// UploadStorageFile sends body to a node's agent on a storage.upload stream
// and returns the agent's verdict. hdr carries the target (StorageID, Rel,
// Staging, Size, Extract, Overwrite); its Kind is set here. The stream is
// half-closed once body ends cleanly, which is the agent's cue that the upload
// is complete; any other end (body read error, ctx canceled) resets it, so the
// agent discards its staging instead of committing a truncated file. An error
// is a transport failure; a refusal comes back as a failed status.
func (h *Hub) UploadStorageFile(ctx context.Context, nodeUUID string, hdr proto.StreamHeader, body io.Reader) (proto.StorageFilesStatus, error) {
	hdr.Kind = proto.StreamStorageUpload
	stream, err := h.openStorageStream(ctx, nodeUUID, hdr)
	if err != nil {
		return proto.StorageFilesStatus{}, err
	}
	return uploadOverStream(ctx, stream, body)
}

// bodyReadError marks an error that came from the caller's body rather than
// the stream.
type bodyReadError struct{ err error }

func (e bodyReadError) Error() string { return "read upload body: " + e.err.Error() }
func (e bodyReadError) Unwrap() error { return e.err }

type sourceReader struct{ r io.Reader }

func (s sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && err != io.EOF {
		err = bodyReadError{err}
	}
	return n, err
}

type uploadAnswer struct {
	status proto.StorageFilesStatus
	err    error
}

func uploadOverStream(ctx context.Context, stream net.Conn, body io.Reader) (proto.StorageFilesStatus, error) {
	committed := false
	defer func() {
		if committed {
			_ = stream.Close()
		} else {
			resetStream(stream)
		}
	}()
	stop := context.AfterFunc(ctx, func() { resetStream(stream) })
	defer stop()

	// The agent may refuse early (size, space, a bad archive) and stop
	// reading; read its answer concurrently so the copy does not block on a
	// full window.
	answer := make(chan uploadAnswer, 1)
	go func() {
		status, err := readStatusLine(stream)
		answer <- uploadAnswer{status: status, err: err}
	}()
	copied := make(chan error, 1)
	go func() {
		// io.Copy would use the stream's ReadFrom if it had one; the wrapper
		// keeps the body's errors apart from the stream's.
		_, err := io.Copy(stream, sourceReader{body})
		if err == nil {
			err = closeWrite(stream)
		}
		copied <- err
	}()

	select {
	case a := <-answer:
		if a.err != nil {
			if ctx.Err() != nil {
				return proto.StorageFilesStatus{}, ctx.Err()
			}
			return proto.StorageFilesStatus{}, fmt.Errorf("read upload answer: %w", a.err)
		}
		if a.status.State != proto.StorageFilesDone {
			// A refusal before the body is through.
			return a.status, nil
		}
		// The agent answers done only after reading the half-close, which
		// the copy sends just before it reports; the two can arrive in
		// either order. A done that precedes the end of the body would claim
		// an upload the agent never saw the end of.
		select {
		case err := <-copied:
			if err != nil {
				return proto.StorageFilesStatus{}, fmt.Errorf("agent reported the upload done but sending it failed: %w", err)
			}
			committed = true
			return a.status, nil
		case <-ctx.Done():
			return proto.StorageFilesStatus{}, ctx.Err()
		}
	case err := <-copied:
		if err != nil {
			var bodyErr bodyReadError
			if errors.As(err, &bodyErr) || ctx.Err() != nil {
				return proto.StorageFilesStatus{}, err
			}
			// The write broke: an early refusal explains it.
			timer := time.NewTimer(storageUploadRefusalWait)
			defer timer.Stop()
			select {
			case a := <-answer:
				if a.err == nil && a.status.State == proto.StorageFilesFailed {
					return a.status, nil
				}
			case <-timer.C:
			}
			return proto.StorageFilesStatus{}, fmt.Errorf("send upload: %w", err)
		}
	}
	// The whole body is sent and the stream half-closed; the agent answers
	// once the result is in place, which for an archive includes unpacking it.
	a := <-answer
	if a.err != nil {
		if ctx.Err() != nil {
			return proto.StorageFilesStatus{}, ctx.Err()
		}
		return proto.StorageFilesStatus{}, fmt.Errorf("read upload answer: %w", a.err)
	}
	committed = true
	return a.status, nil
}

func readStatusLine(r io.Reader) (proto.StorageFilesStatus, error) {
	line, err := readAnswerLine(bufio.NewReaderSize(r, storageHeadLineMax))
	if err != nil {
		return proto.StorageFilesStatus{}, err
	}
	var status proto.StorageFilesStatus
	if err := json.Unmarshal(line, &status); err != nil {
		return status, fmt.Errorf("bad upload answer %q: %w", strings.TrimSpace(string(line)), err)
	}
	if status.State != proto.StorageFilesDone && status.State != proto.StorageFilesFailed {
		return status, fmt.Errorf("upload answer has unknown state %q", status.State)
	}
	return status, nil
}

// readAnswerLine reads one newline-terminated line of at most the reader's buffer
// size.
func readAnswerLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, errors.New("answer line too long")
	}
	if err != nil {
		return nil, err
	}
	return line, nil
}

// OpenStorageDownload opens a storage.download stream and reads the agent's
// head line. hdr carries StorageID, Rel and, for a directory, Format; its Kind
// is set here. A refusal comes back as a head with Code set and no body. On
// success the body is the bytes after the head; closing it before its end, or
// ctx ending, resets the stream so the agent stops packing.
func (h *Hub) OpenStorageDownload(ctx context.Context, nodeUUID string, hdr proto.StreamHeader) (proto.StorageDownloadHead, io.ReadCloser, error) {
	hdr.Kind = proto.StreamStorageDownload
	stream, err := h.openStorageStream(ctx, nodeUUID, hdr)
	if err != nil {
		return proto.StorageDownloadHead{}, nil, err
	}
	return readDownloadHead(ctx, stream, storageDownloadHeadTimeout)
}

func readDownloadHead(ctx context.Context, stream net.Conn, timeout time.Duration) (proto.StorageDownloadHead, io.ReadCloser, error) {
	stop := context.AfterFunc(ctx, func() { resetStream(stream) })
	timer := time.AfterFunc(timeout, func() { resetStream(stream) })
	reader := bufio.NewReaderSize(stream, storageHeadLineMax)
	line, err := readAnswerLine(reader)
	timedOut := !timer.Stop()
	fail := func(err error) (proto.StorageDownloadHead, io.ReadCloser, error) {
		stop()
		resetStream(stream)
		return proto.StorageDownloadHead{}, nil, err
	}
	if err != nil {
		if ctx.Err() != nil {
			return fail(ctx.Err())
		}
		if timedOut {
			return fail(errors.New("agent did not answer the download"))
		}
		return fail(fmt.Errorf("read download head: %w", err))
	}
	var head proto.StorageDownloadHead
	if err := json.Unmarshal(line, &head); err != nil {
		return fail(fmt.Errorf("bad download head %q: %w", strings.TrimSpace(string(line)), err))
	}
	if head.Code != "" || head.Error != "" {
		stop()
		_ = stream.Close()
		if head.Code == "" {
			head.Code = proto.StorageFilesErrFailed
		}
		return head, nil, nil
	}
	return head, &downloadBody{reader: reader, stream: stream, stop: stop}, nil
}

// downloadBody is a download's bytes after the head line.
type downloadBody struct {
	reader *bufio.Reader
	stream net.Conn
	stop   func() bool
	once   sync.Once
	mu     sync.Mutex
	eof    bool
}

func (b *downloadBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if err == io.EOF {
		b.mu.Lock()
		b.eof = true
		b.mu.Unlock()
	}
	return n, err
}

// Close ends the download. Before EOF it resets the stream: a plain yamux
// Close only half-closes, and the agent would keep packing into a window
// nobody drains.
func (b *downloadBody) Close() error {
	b.once.Do(func() {
		b.stop()
		b.mu.Lock()
		eof := b.eof
		b.mu.Unlock()
		if eof {
			_ = b.stream.Close()
			return
		}
		resetStream(b.stream)
	})
	return nil
}

// resetStream aborts a stream in both directions. yamux's Close sends FIN,
// which the other side reads as a clean end; an abandoned upload or download
// must look like the failure it is.
func resetStream(c net.Conn) {
	if r, ok := c.(interface{ Reset() error }); ok {
		_ = r.Reset()
		return
	}
	_ = c.Close()
}

func closeWrite(c net.Conn) error {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.New("stream does not support a half-close")
}
