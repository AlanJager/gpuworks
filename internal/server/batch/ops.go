package batch

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Upload saves and validates an input file (purpose "batch", the only one).
// A refused file leaves nothing behind.
func (s *Service) Upload(owner Owner, filename string, src io.Reader) (File, error) {
	if err := os.MkdirAll(s.cfg.Dir, 0o750); err != nil {
		return File{}, err
	}
	tmp, err := os.CreateTemp(s.cfg.Dir, "upload-*.tmp")
	if err != nil {
		return File{}, err
	}
	defer os.Remove(tmp.Name())
	stats, verr := scanInput(src, tmp)
	if cerr := tmp.Close(); verr == nil && cerr != nil {
		return File{}, cerr
	}
	if verr != nil {
		return File{}, verr
	}
	f := File{
		ID: randomID("file-"), UserID: owner.UserID, APIKeyID: owner.APIKeyID, Purpose: PurposeBatch,
		Filename: cleanFilename(filename), Bytes: stats.Bytes, Lines: stats.Lines, Endpoint: stats.Endpoint,
		CreatedAt: s.cfg.Clock.Now().UTC(),
	}
	dst, _ := s.filePath(f.ID)
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return File{}, err
	}
	if err := s.repo.CreateFile(f); err != nil {
		_ = os.Remove(dst)
		return File{}, err
	}
	return f, nil
}

// cleanFilename keeps the base name of an uploaded file, within the column.
func cleanFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	if name == "." || name == "/" || name == "" {
		name = "batch.jsonl"
	}
	for len(name) > 255 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

// File reads a file's metadata; a file of another user is not found.
func (s *Service) File(owner Owner, id string) (File, error) {
	f, err := s.repo.GetFile(id)
	if errors.Is(err, ErrNotFound) || (err == nil && !owner.Sees(f.UserID)) {
		return File{}, notFound("file_not_found", "No such File object: "+id)
	}
	return f, err
}

// OpenFile opens a file's bytes. A file whose bytes are gone (removed with
// the retention) is not found.
func (s *Service) OpenFile(owner Owner, id string) (File, *os.File, error) {
	f, err := s.File(owner, id)
	if err != nil {
		return File{}, nil, err
	}
	path, err := s.filePath(f.ID)
	if err != nil {
		return File{}, nil, notFound("file_not_found", "No such File object: "+id)
	}
	fh, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return File{}, nil, notFound("file_not_found", "The file's content is no longer available: "+id)
		}
		return File{}, nil, err
	}
	return f, fh, nil
}

// ListFiles lists the owner's files newest first; more is true when further
// files follow the returned ones.
func (s *Service) ListFiles(owner Owner, purpose, after string, limit int) (files []File, more bool, err error) {
	files, err = s.repo.ListFiles(owner, purpose, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	if len(files) > limit {
		files, more = files[:limit], true
	}
	return files, more, nil
}

// DeleteFile removes a file, refused while a batch that has not finished
// reads it.
func (s *Service) DeleteFile(owner Owner, id string) error {
	f, err := s.File(owner, id)
	if err != nil {
		return err
	}
	switch err := s.repo.DeleteFile(f.ID); {
	case errors.Is(err, ErrFileInUse):
		return &Error{Status: 409, Type: "invalid_request_error", Code: "file_in_use", Message: "A batch that has not finished uses this file: " + id}
	case errors.Is(err, ErrNotFound):
		return notFound("file_not_found", "No such File object: "+id)
	case err != nil:
		return err
	}
	if path, err := s.filePath(f.ID); err == nil {
		_ = os.Remove(path)
	}
	return nil
}

// CreateRequest is a batch to create.
type CreateRequest struct {
	InputFileID      string
	Endpoint         string
	CompletionWindow string
	Metadata         map[string]string
}

// CreateBatch validates the request against the input file and starts the
// batch.
func (s *Service) CreateBatch(owner Owner, req CreateRequest) (Batch, error) {
	if req.InputFileID == "" {
		return Batch{}, invalid("missing_required_parameter", "input_file_id", "input_file_id is required")
	}
	if !slices.Contains(Endpoints, req.Endpoint) {
		return Batch{}, invalid("invalid_endpoint", "endpoint", "endpoint must be one of "+strings.Join(Endpoints, ", "))
	}
	if req.CompletionWindow != CompletionWindow {
		return Batch{}, invalid("invalid_completion_window", "completion_window", `completion_window must be "24h"`)
	}
	if err := checkMetadata(req.Metadata); err != nil {
		return Batch{}, err
	}
	in, err := s.repo.GetFile(req.InputFileID)
	if errors.Is(err, ErrNotFound) || (err == nil && !owner.Sees(in.UserID)) {
		return Batch{}, invalid("file_not_found", "input_file_id", "No such File object: "+req.InputFileID)
	}
	if err != nil {
		return Batch{}, err
	}
	if in.Purpose != PurposeBatch {
		return Batch{}, invalid("invalid_purpose", "input_file_id", `the input file's purpose must be "batch"`)
	}
	if in.Endpoint != req.Endpoint {
		message := "the input file's requests do not all ask for " + req.Endpoint
		if in.Endpoint != "" {
			message = "the input file's requests ask for " + in.Endpoint + ", not " + req.Endpoint
		}
		return Batch{}, invalid("endpoint_mismatch", "endpoint", message)
	}
	now := s.cfg.Clock.Now().UTC()
	b := Batch{
		ID: randomID("batch_"), UserID: owner.UserID, APIKeyID: owner.APIKeyID, Endpoint: req.Endpoint,
		InputFileID: in.ID, CompletionWindow: CompletionWindow, Status: StatusValidating, Total: in.Lines,
		Metadata: req.Metadata, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour),
	}
	if err := s.repo.CreateBatch(b); err != nil {
		return Batch{}, err
	}
	s.launch(b)
	return b, nil
}

func checkMetadata(m map[string]string) error {
	if len(m) > MaxMetadataPairs {
		return invalid("invalid_metadata", "metadata", "metadata has more than 16 keys")
	}
	for k, v := range m {
		if k == "" || utf8.RuneCountInString(k) > MaxMetadataKey {
			return invalid("invalid_metadata", "metadata", "a metadata key must be 1 to 64 characters")
		}
		if utf8.RuneCountInString(v) > MaxMetadataValue {
			return invalid("invalid_metadata", "metadata", "a metadata value must be at most 512 characters")
		}
	}
	return nil
}

// Batch reads a batch; a batch of another user is not found.
func (s *Service) Batch(owner Owner, id string) (Batch, error) {
	b, err := s.repo.GetBatch(id)
	if errors.Is(err, ErrNotFound) || (err == nil && !owner.Sees(b.UserID)) {
		return Batch{}, notFound("batch_not_found", "No such Batch object: "+id)
	}
	return b, err
}

// ListBatches lists the owner's batches newest first after the batch named by
// after; more is true when further batches follow.
func (s *Service) ListBatches(owner Owner, after string, limit int) (batches []Batch, more bool, err error) {
	batches, err = s.repo.ListBatchesAfter(owner, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	if len(batches) > limit {
		batches, more = batches[:limit], true
	}
	return batches, more, nil
}

// ListBatchesPage lists one page of the owner's batches, those in one of
// statuses when any is given, with the number of batches that match.
func (s *Service) ListBatchesPage(owner Owner, statuses []string, offset, limit int) ([]Batch, int, error) {
	return s.repo.ListBatchesPage(owner, statuses, offset, limit)
}

// Cancel stops a batch that has not finished: it goes to cancelling, lets the
// requests in flight finish for a while and ends as cancelled with the results
// so far. A batch already cancelling is returned as it is.
func (s *Service) Cancel(owner Owner, id string) (Batch, error) {
	if _, err := s.Batch(owner, id); err != nil {
		return Batch{}, err
	}
	b, err := s.repo.RequestCancel(id, s.cfg.Clock.Now())
	if errors.Is(err, ErrTerminal) {
		return Batch{}, &Error{Status: 409, Type: "invalid_request_error", Code: "batch_not_cancellable", Message: "The batch is " + b.Status + " and cannot be cancelled: " + id}
	}
	if err != nil {
		return Batch{}, err
	}
	s.signalCancel(b)
	return b, nil
}
