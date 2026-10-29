package app

import (
	"errors"
	"mime/multipart"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/batch"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// batchConsole is the console's side of the Batch API (api.BatchService) over
// the batch service.
type batchConsole struct {
	svc *batch.Service
}

func consoleOwner(c api.BatchCaller) batch.Owner {
	return batch.Owner{UserID: c.UserID, All: c.All}
}

func (c batchConsole) List(caller api.BatchCaller, states []string, page, pageSize int) (dto.BatchPage, error) {
	rows, total, err := c.svc.ListBatchesPage(consoleOwner(caller), states, (page-1)*pageSize, pageSize)
	if err != nil {
		return dto.BatchPage{}, consoleBatchError(err)
	}
	items := make([]dto.Batch, 0, len(rows))
	for _, b := range rows {
		items = append(items, batchDTO(b, ""))
	}
	return dto.BatchPage{Items: items, Page: page, PageSize: pageSize, Total: total}, nil
}

func (c batchConsole) Get(caller api.BatchCaller, id string) (dto.Batch, error) {
	b, err := c.svc.Batch(consoleOwner(caller), id)
	if err != nil {
		return dto.Batch{}, consoleBatchError(err)
	}
	return c.detail(b), nil
}

func (c batchConsole) Cancel(caller api.BatchCaller, id string) (dto.Batch, error) {
	b, err := c.svc.Cancel(consoleOwner(caller), id)
	if err != nil {
		return dto.Batch{}, consoleBatchError(err)
	}
	return c.detail(b), nil
}

func (c batchConsole) CreateFromUpload(caller api.BatchCaller, form *multipart.Reader) (dto.Batch, error) {
	b, err := c.svc.CreateFromUpload(consoleOwner(caller), form)
	if err != nil {
		return dto.Batch{}, consoleBatchError(err)
	}
	return c.detail(b), nil
}

func (c batchConsole) Download(caller api.BatchCaller, id, kind string) (api.BatchDownload, error) {
	f, fh, err := c.svc.OpenBatchFile(consoleOwner(caller), id, kind)
	if err != nil {
		return api.BatchDownload{}, consoleBatchError(err)
	}
	return api.BatchDownload{Filename: f.Filename, Size: f.Bytes, Body: fh}, nil
}

// detail is a batch with the name of its input file, which a list row leaves out.
func (c batchConsole) detail(b batch.Batch) dto.Batch {
	name := ""
	if f, err := c.svc.FileOf(b.InputFileID); err == nil {
		name = f.Filename
	}
	return batchDTO(b, name)
}

func batchDTO(b batch.Batch, inputName string) dto.Batch {
	out := dto.Batch{
		ID: b.ID, Endpoint: b.Endpoint, Status: b.Status, InputFileID: b.InputFileID, InputFilename: inputName,
		OutputFileID: b.OutputFileID, ErrorFileID: b.ErrorFileID, CompletionWindow: b.CompletionWindow,
		RequestCounts: dto.BatchRequestCount{Total: b.Total, Completed: b.Completed, Failed: b.Failed},
		Model:         b.Model, Metadata: map[string]string{}, UserID: b.UserID, APIKeyID: b.APIKeyID,
		CreatedAt: b.CreatedAt, ExpiresAt: b.ExpiresAt, InProgressAt: b.InProgressAt, FinalizingAt: b.FinalizingAt,
		CompletedAt: b.CompletedAt, FailedAt: b.FailedAt, ExpiredAt: b.ExpiredAt, CancellingAt: b.CancellingAt, CancelledAt: b.CancelledAt,
	}
	for k, v := range b.Metadata {
		out.Metadata[k] = v
	}
	for _, w := range b.Warnings {
		if w.Code == batch.WarningModelSleepHeld {
			out.Warnings = append(out.Warnings, messages.Warning{Code: api.WarningBatchModelSleepHeld, Params: map[string]any{"model": b.Model}})
		}
	}
	if b.ErrorCode != "" {
		out.Failure = &dto.BatchFailure{Code: b.ErrorCode, Message: b.ErrorMessage}
	}
	return out
}

// consoleBatchError turns a batch refusal into the console's product error:
// not found, not cancellable, a bad file (with its line), a file over the
// limit, or a bad request field.
func consoleBatchError(err error) error {
	var be *batch.Error
	if !errors.As(err, &be) {
		return err
	}
	out := &api.BatchError{Status: be.Status, Fallback: be.Message}
	switch {
	case be.Status == http.StatusNotFound:
		out.Code = api.ErrorBatchNotFound
	case be.Code == "model_not_found":
		out.Code = api.ErrorBatchModelUnknown
		out.Params = map[string]any{"model": be.Model}
	case be.Code == "batch_not_cancellable":
		out.Code = api.ErrorBatchNotCancellable
	case be.Status == http.StatusRequestEntityTooLarge:
		out.Code = api.ErrorBatchFileTooLarge
		out.Params = map[string]any{"limit_mb": batch.MaxFileBytes >> 20}
	case be.Line > 0 || be.Code == "empty_file" || be.Code == "too_many_requests" || be.Code == "invalid_file_extension":
		out.Code = api.ErrorBatchFileInvalid
		out.Params = map[string]any{"line": be.Line, "reason": be.Code, "detail": be.Message}
	default:
		out.Code = api.ErrorBatchRequestInvalid
		out.Params = map[string]any{"field": be.Param, "reason": be.Code}
	}
	return out
}
