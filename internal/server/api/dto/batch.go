package dto

import (
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Batch is an asynchronous batch of requests as the console's API shows it
// (GET /batches, GET /batches/{id}; the OpenAI-compatible /v1/batches has its
// own wire shape, docs/reference/api.md). Status is validating, in_progress,
// finalizing, completed, failed, expired, cancelling or cancelled. The files
// are named only once they exist: the output file holds the requests that
// succeeded, the error file those that failed; both are read through
// GET /batches/{id}/files/{kind}. Times the batch has not reached are left out.
type Batch struct {
	ID       string `json:"id"`
	Endpoint string `json:"endpoint"`
	Status   string `json:"status"`
	// Cancellable: POST /batches/{id}/cancel would move the batch to
	// cancelling (validating or in_progress); the server decides, the client
	// only shows it.
	Cancellable      bool   `json:"cancellable"`
	InputFileID      string `json:"input_file_id"`
	InputFilename    string `json:"input_filename,omitempty"`
	OutputFileID     string `json:"output_file_id,omitempty"`
	ErrorFileID      string `json:"error_file_id,omitempty"`
	CompletionWindow string `json:"completion_window"`
	// Model is the served name every request of the batch is sent to, replacing
	// the model of its lines; absent when each line names its own.
	Model         string            `json:"model,omitempty"`
	RequestCounts BatchRequestCount `json:"request_counts"`
	Metadata      map[string]string `json:"metadata"`
	// Failure says why a failed batch failed.
	Failure *BatchFailure `json:"failure,omitempty"`
	// OutputExpiresAt is when the output and error files are removed, the
	// server's retention after the batch ended; absent while it runs or when
	// it wrote neither file. A time in the past means they are gone.
	OutputExpiresAt *time.Time `json:"output_expires_at,omitempty"`
	// Warnings are the non-blocking findings of creating the batch, on the
	// create response only: BATCH_MODEL_SLEEP_HELD {model} for the model the
	// batch was given, BATCH_FILE_MODELS_SLEEP_HELD {models, more} for the
	// models its input file names.
	Warnings []messages.Warning `json:"warnings,omitempty"`
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

// BatchDictionary is what the console offers a batch with
// (GET /batches/dictionary): the endpoints a batch may call, the completion
// windows it may ask for, the limits of its input file, the statuses it moves
// through in the order a list filters them, how many days its output and
// error files are kept, and the served names it can be sent to. The server
// enforces every one of them; a client only shows them.
type BatchDictionary struct {
	Endpoints             []string         `json:"endpoints"`
	CompletionWindowHours BatchWindowHours `json:"completion_window_hours"`
	MaxFileBytes          int64            `json:"max_file_bytes"`
	MaxFileLines          int              `json:"max_file_lines"`
	Statuses              []string         `json:"statuses"`
	OutputRetentionDays   int              `json:"output_retention_days"`
	// Models are the served names of the active deployments, the ones that
	// serve first and then by name. A create names one of them, or a file
	// whose lines do, or it is refused; a held one is accepted with a warning.
	Models []BatchModel `json:"models"`
}

// BatchWindowHours bounds a batch's completion window in whole hours and
// gives the one a batch gets when it asks for none.
type BatchWindowHours struct {
	Min     int `json:"min"`
	Max     int `json:"max"`
	Default int `json:"default"`
}

// The states of a BatchModel.
const (
	// BatchModelServed: a deployment serving the name is awake (or starting).
	BatchModelServed = "served"
	// BatchModelAsleep: it sleeps, and a request wakes it.
	BatchModelAsleep = "asleep"
	// BatchModelAsleepHeld: it sleeps by an operator's or a schedule's order,
	// which a request does not wake; a batch sent to it waits and retries.
	BatchModelAsleepHeld = "asleep_held"
)

// BatchModel is a served name a batch can be sent to and what it is now
// (BatchModelServed, BatchModelAsleep or BatchModelAsleepHeld). A name several
// deployments share is as good as the best of them.
type BatchModel struct {
	Name  string `json:"name"`
	State string `json:"state"`
}
