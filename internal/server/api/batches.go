package api

import (
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// Batch codes (GET /batches, POST /batches, the Batch API's console side).
const (
	// ErrorBatchNotFound: no such batch, file or output file for the caller.
	ErrorBatchNotFound = "BATCH_NOT_FOUND"
	// ErrorBatchNotCancellable: the batch has finished ({status}).
	ErrorBatchNotCancellable = "BATCH_NOT_CANCELLABLE"
	// ErrorBatchFileInvalid: the uploaded file is not a valid batch input:
	// {line} of it (0 for the file as a whole), {reason} the check that failed
	// and {detail} its English text.
	ErrorBatchFileInvalid = "BATCH_FILE_INVALID"
	// ErrorBatchFileTooLarge: the file is over the {limit_mb} megabytes allowed.
	ErrorBatchFileTooLarge = "BATCH_FILE_TOO_LARGE"
	// ErrorBatchRequestInvalid: a field of the request ({field}) is not valid.
	ErrorBatchRequestInvalid = "BATCH_REQUEST_INVALID"
)

// BatchService is the Batch API as the console uses it: the batches of
// every user for an administrator, of the caller's own for anyone else. The
// app implements it over internal/server/batch; the OpenAI-compatible /v1
// routes are served by that package directly, under the gateway.
type BatchService interface {
	List(c BatchCaller, states []string, page, pageSize int) (dto.BatchPage, error)
	Get(c BatchCaller, id string) (dto.Batch, error)
	Cancel(c BatchCaller, id string) (dto.Batch, error)
	// CreateFromUpload reads a multipart form with a "file" (the JSONL input)
	// and an "endpoint", validates the file while saving it and starts the batch.
	CreateFromUpload(c BatchCaller, form *multipart.Reader) (dto.Batch, error)
	// Download opens the input, output or error file of a batch (kind "input",
	// "output" or "error").
	Download(c BatchCaller, id, kind string) (BatchDownload, error)
}

// BatchCaller is who a console request acts for.
type BatchCaller struct {
	UserID uint
	// All: the caller sees every user's batches.
	All bool
}

// BatchDownload is a batch file ready to be sent.
type BatchDownload struct {
	Filename string
	Size     int64
	Body     io.ReadCloser
}

// BatchError is a refused batch request, carried to the client as a product
// error.
type BatchError struct {
	Status   int
	Code     string
	Fallback string
	Params   map[string]any
}

func (e *BatchError) Error() string { return e.Fallback }

var errBatchesUnavailable = errors.New("the batch service is not available")

var batchStates = []string{"validating", "in_progress", "finalizing", "completed", "failed", "expired", "cancelling", "cancelled"}

func (a *apiHandlers) batchCaller(r *http.Request) BatchCaller {
	claims, _ := a.claimsFromRequest(r)
	return BatchCaller{UserID: claims.UserID, All: claims.Role == "admin"}
}

func (a *apiHandlers) writeBatchError(w http.ResponseWriter, err error) {
	var be *BatchError
	if errors.As(err, &be) {
		writeProductError(w, be.Status, be.Code, be.Fallback, be.Params)
		return
	}
	writeErr(w, http.StatusInternalServerError, err)
}

// listBatches serves GET /api/v1/batches?page=&page_size=&state=, newest first.
func (a *apiHandlers) listBatches(w http.ResponseWriter, r *http.Request) {
	if a.d.Batches == nil {
		writeErr(w, http.StatusNotFound, errBatchesUnavailable)
		return
	}
	q, err := parseListQuery(r, batchStates)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if q.search != "" {
		writeErr(w, http.StatusBadRequest, listFilterError("q", q.search))
		return
	}
	var states []string
	for _, s := range batchStates {
		if q.states[s] {
			states = append(states, s)
		}
	}
	page, err := a.d.Batches.List(a.batchCaller(r), states, q.page, q.pageSize)
	if err != nil {
		a.writeBatchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// getBatch serves GET /api/v1/batches/{id}.
func (a *apiHandlers) getBatch(w http.ResponseWriter, r *http.Request) {
	if a.d.Batches == nil {
		writeErr(w, http.StatusNotFound, errBatchesUnavailable)
		return
	}
	b, err := a.d.Batches.Get(a.batchCaller(r), chi.URLParam(r, "id"))
	if err != nil {
		a.writeBatchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// cancelBatch serves POST /api/v1/batches/{id}/cancel.
func (a *apiHandlers) cancelBatch(w http.ResponseWriter, r *http.Request) {
	if a.d.Batches == nil {
		writeErr(w, http.StatusNotFound, errBatchesUnavailable)
		return
	}
	b, err := a.d.Batches.Cancel(a.batchCaller(r), chi.URLParam(r, "id"))
	if err != nil {
		a.writeBatchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// createBatch serves POST /api/v1/batches: a multipart form with the input
// file and its endpoint.
func (a *apiHandlers) createBatch(w http.ResponseWriter, r *http.Request) {
	if a.d.Batches == nil {
		writeErr(w, http.StatusNotFound, errBatchesUnavailable)
		return
	}
	if mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		writeProductError(w, http.StatusBadRequest, ErrorBatchRequestInvalid, "the request must be multipart/form-data with the fields file and endpoint", map[string]any{"field": "file"})
		return
	}
	form, err := r.MultipartReader()
	if err != nil {
		writeProductError(w, http.StatusBadRequest, ErrorBatchRequestInvalid, "the multipart body cannot be read: "+err.Error(), map[string]any{"field": "file"})
		return
	}
	b, err := a.d.Batches.CreateFromUpload(a.batchCaller(r), form)
	if err != nil {
		a.writeBatchError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

// downloadBatchFile serves GET /api/v1/batches/{id}/files/{kind}: the
// batch's input, output or error file as JSON Lines.
func (a *apiHandlers) downloadBatchFile(w http.ResponseWriter, r *http.Request) {
	if a.d.Batches == nil {
		writeErr(w, http.StatusNotFound, errBatchesUnavailable)
		return
	}
	kind := chi.URLParam(r, "kind")
	if kind != "input" && kind != "output" && kind != "error" {
		writeProductError(w, http.StatusBadRequest, ErrorBatchRequestInvalid, "kind must be input, output or error", map[string]any{"field": "kind"})
		return
	}
	file, err := a.d.Batches.Download(a.batchCaller(r), chi.URLParam(r, "id"), kind)
	if err != nil {
		a.writeBatchError(w, err)
		return
	}
	defer file.Body.Close()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": file.Filename}))
	w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file.Body)
}
