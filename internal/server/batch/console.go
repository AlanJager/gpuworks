package batch

import (
	"errors"
	"io"
	"mime/multipart"
	"os"
	"strings"
)

// CreateFromUpload creates a batch from the console's form: a "file" with the
// JSONL requests and an "endpoint", in either order, and optionally a
// "completion_window" (24h by default, 1h to 72h) and a "model" that every request is sent to. The file is validated while it is saved; one that
// is refused, or an endpoint it does not fit, leaves nothing behind.
func (s *Service) CreateFromUpload(owner Owner, mr *multipart.Reader) (Batch, error) {
	var (
		endpoint, window, model string
		saved                   *File
	)
	window = CompletionWindow
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.discard(owner, saved)
			return Batch{}, invalid("invalid_multipart", "", "the multipart body cannot be read: "+err.Error())
		}
		switch part.FormName() {
		case "endpoint", "completion_window", "model":
			raw, _ := io.ReadAll(io.LimitReader(part, 256))
			switch part.FormName() {
			case "endpoint":
				endpoint = strings.TrimSpace(string(raw))
			case "model":
				model = strings.TrimSpace(string(raw))
			default:
				window = strings.TrimSpace(string(raw))
			}
		case "file":
			if saved != nil {
				s.discard(owner, saved)
				return Batch{}, invalid("invalid_multipart", "file", "the request has more than one file")
			}
			f, err := s.Upload(owner, part.FileName(), part)
			if err != nil {
				return Batch{}, err
			}
			saved = &f
		}
	}
	if saved == nil {
		return Batch{}, invalid("missing_required_parameter", "file", "file is required")
	}
	b, err := s.CreateBatch(owner, CreateRequest{InputFileID: saved.ID, Endpoint: endpoint, CompletionWindow: window, Model: model})
	if err != nil {
		s.discard(owner, saved)
		return Batch{}, err
	}
	return b, nil
}

// OpenBatchFile opens the input, output or error file of a batch for its
// owner: kind is "input", "output" or "error". The file's metadata comes
// with it.
func (s *Service) OpenBatchFile(owner Owner, batchID, kind string) (File, *os.File, error) {
	b, err := s.Batch(owner, batchID)
	if err != nil {
		return File{}, nil, err
	}
	id := map[string]string{"input": b.InputFileID, "output": b.OutputFileID, "error": b.ErrorFileID}[kind]
	if id == "" {
		return File{}, nil, notFound("file_not_found", "The batch has no "+kind+" file")
	}
	// The files of a batch are its owner's by definition, whoever asks.
	return s.OpenFile(Owner{All: true}, id)
}

// FileOf reads a file's metadata without checking who asks; for a caller
// that has checked access through the batch that names it.
func (s *Service) FileOf(id string) (File, error) {
	f, err := s.repo.GetFile(id)
	if errors.Is(err, ErrNotFound) {
		return File{}, notFound("file_not_found", "No such File object: "+id)
	}
	return f, err
}
