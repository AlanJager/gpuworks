package dto

import "time"

// Batch is an asynchronous batch of requests as the console's API shows it
// (GET /batches, GET /batches/{id}; the OpenAI-compatible /v1/batches has its
// own wire shape, docs/reference/api.md). Status is validating, in_progress,
// finalizing, completed, failed, expired, cancelling or cancelled. The files
// are named only once they exist: the output file holds the requests that
// succeeded, the error file those that failed; both are read through
// GET /batches/{id}/files/{kind}. Times the batch has not reached are left out.
type Batch struct {
	ID               string            `json:"id"`
	Endpoint         string            `json:"endpoint"`
	Status           string            `json:"status"`
	InputFileID      string            `json:"input_file_id"`
	InputFilename    string            `json:"input_filename,omitempty"`
	OutputFileID     string            `json:"output_file_id,omitempty"`
	ErrorFileID      string            `json:"error_file_id,omitempty"`
	CompletionWindow string            `json:"completion_window"`
	RequestCounts    BatchRequestCount `json:"request_counts"`
	Metadata         map[string]string `json:"metadata"`
	// Failure says why a failed batch failed.
	Failure *BatchFailure `json:"failure,omitempty"`
	// UserID is the user that created the batch, through an API key or the console.
	UserID       uint       `json:"user_id"`
	APIKeyID     uint       `json:"api_key_id,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	InProgressAt *time.Time `json:"in_progress_at,omitempty"`
	FinalizingAt *time.Time `json:"finalizing_at,omitempty"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	FailedAt     *time.Time `json:"failed_at,omitempty"`
	ExpiredAt    *time.Time `json:"expired_at,omitempty"`
	CancellingAt *time.Time `json:"cancelling_at,omitempty"`
	CancelledAt  *time.Time `json:"cancelled_at,omitempty"`
}

// BatchRequestCount is a batch's progress: the requests in its input file, the
// ones that have succeeded and the ones that failed for good. Requests the
// platform retries are in neither.
type BatchRequestCount struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
}

// BatchFailure is the reason a batch failed.
type BatchFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// BatchPage is one page of GET /api/v1/batches, newest first.
type BatchPage = ListPage[Batch]
