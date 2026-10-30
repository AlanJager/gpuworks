// Package batch is the OpenAI-compatible Batch API: an uploaded JSONL file of
// requests is run asynchronously through the gateway, with the results kept in
// an output file and the failures in an error file (docs/how-to/batch-api.md).
//
// The package knows neither the store nor the gateway: it persists through
// Repo and sends through Sender, which the app implements over them, so the
// executor is tested with a fake clock, a fake sender and an in-memory Repo.
package batch

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Statuses of a batch, as the OpenAI Batch API names them.
const (
	StatusValidating = "validating"
	StatusFailed     = "failed"
	StatusInProgress = "in_progress"
	StatusFinalizing = "finalizing"
	StatusCompleted  = "completed"
	StatusExpired    = "expired"
	StatusCancelling = "cancelling"
	StatusCancelled  = "cancelled"
)

// Terminal reports whether a batch in status has finished for good.
func Terminal(status string) bool {
	switch status {
	case StatusFailed, StatusCompleted, StatusExpired, StatusCancelled:
		return true
	}
	return false
}

// Purposes of a file: the OpenAI Files API's names.
const (
	PurposeBatch  = "batch"
	PurposeOutput = "batch_output"
)

// Endpoints a batch may call, and the only completion window.
const (
	EndpointChat       = "/v1/chat/completions"
	EndpointCompletion = "/v1/completions"
	EndpointEmbedding  = "/v1/embeddings"
	CompletionWindow   = "24h"
)

// The completion windows a batch may ask for, in whole hours: "24h" is OpenAI's
// and the default, "1h" to "72h" are a gpuworks extension.
const (
	MinWindowHours = 1
	MaxWindowHours = 72
)

// ParseWindow reads a completion window of the form "<hours>h" with whole
// hours from MinWindowHours to MaxWindowHours, written without leading zeros.
func ParseWindow(s string) (time.Duration, bool) {
	hours, found := strings.CutSuffix(s, "h")
	n, err := strconv.Atoi(hours)
	if !found || err != nil || strconv.Itoa(n) != hours || n < MinWindowHours || n > MaxWindowHours {
		return 0, false
	}
	return time.Duration(n) * time.Hour, true
}

// Endpoints is the set of urls an input file's requests may ask for.
var Endpoints = []string{EndpointChat, EndpointCompletion, EndpointEmbedding}

// Limits of an input file and of a batch's metadata (OpenAI's).
const (
	MaxFileBytes     = 200 << 20
	MaxFileLines     = 50000
	MaxLineBytes     = 16 << 20
	MaxMetadataPairs = 16
	MaxMetadataKey   = 64
	MaxMetadataValue = 512
)

// Owner is who a request acts for. A caller sees the files and batches its
// user created; an administrator of the console (All) sees every user's.
type Owner struct {
	UserID   uint
	APIKeyID uint
	All      bool
}

// Sees reports whether the owner may see a row created by userID.
func (o Owner) Sees(userID uint) bool { return o.All || o.UserID == userID }

// File is a stored file's metadata.
type File struct {
	ID        string
	UserID    uint
	APIKeyID  uint
	Purpose   string
	Filename  string
	Bytes     int64
	Lines     int
	Endpoint  string
	CreatedAt time.Time
}

// Batch is a batch's state.
type Batch struct {
	ID               string
	UserID           uint
	APIKeyID         uint
	Endpoint         string
	InputFileID      string
	OutputFileID     string
	ErrorFileID      string
	CompletionWindow string
	// Model is the served name every request is sent to, replacing its line's
	// own body.model; empty when each line names its own.
	Model            string
	Status           string
	ErrorCode        string
	ErrorMessage     string
	Total            int
	Completed        int
	Failed           int
	CheckpointOffset int64
	Metadata         map[string]string
	CreatedAt        time.Time
	ExpiresAt        time.Time
	InProgressAt     *time.Time
	FinalizingAt     *time.Time
	CompletedAt      *time.Time
	FailedAt         *time.Time
	ExpiredAt        *time.Time
	CancellingAt     *time.Time
	CancelledAt      *time.Time
	// Warnings are the non-blocking findings of creating the batch; set on the
	// batch CreateBatch returns, never stored.
	Warnings []Warning
}

// Warning codes of a created batch.
const (
	// WarningModelSleepHeld: the model's deployment sleeps by an operator's or
	// a schedule's order, which a request does not wake; its requests are
	// retried until someone wakes it or the batch expires.
	WarningModelSleepHeld = "model_sleep_held"
)

// Warning is a finding about a batch that does not refuse it.
type Warning struct {
	Code    string
	Message string
}

// Owner is the owner a batch acts for when it sends requests.
func (b Batch) Owner() Owner { return Owner{UserID: b.UserID, APIKeyID: b.APIKeyID} }

// Progress is an executor checkpoint: the requests with a result so far and
// the byte length of the partial result file that holds them.
type Progress struct {
	Completed, Failed int
	Offset            int64
}

// Finish is how a batch ends. Output and Error are the files written for it,
// nil for none.
type Finish struct {
	Status       string
	At           time.Time
	Completed    int
	Failed       int
	ErrorCode    string
	ErrorMessage string
	Output       *File
	Error        *File
}

// Errors a Repo reports.
var (
	ErrNotFound  = errors.New("not found")
	ErrFileInUse = errors.New("a batch that has not finished uses the file")
	ErrTerminal  = errors.New("the batch has finished")
)

// Repo persists files and batches. Its writes are per state change or per
// checkpoint, never per request.
type Repo interface {
	CreateFile(f File) error
	GetFile(id string) (File, error)
	ListFiles(owner Owner, purpose, after string, limit int) ([]File, error)
	DeleteFile(id string) error
	CreateBatch(b Batch) error
	GetBatch(id string) (Batch, error)
	ListBatchesAfter(owner Owner, after string, limit int) ([]Batch, error)
	ListBatchesPage(owner Owner, statuses []string, offset, limit int) ([]Batch, int, error)
	Unfinished() ([]Batch, error)
	Start(id string, at time.Time) (bool, error)
	SaveProgress(id string, p Progress) error
	RequestCancel(id string, at time.Time) (Batch, error)
	BeginFinalize(id string, at time.Time) error
	Finish(id string, f Finish) error
	DeleteOutputsBefore(cutoff time.Time) ([]string, error)
}

// ModelState is what a served model name is on the platform.
type ModelState int

const (
	// ModelUnknown: no active deployment serves the name.
	ModelUnknown ModelState = iota
	// ModelAvailable: an active deployment serves it, running or asleep in a
	// way a request wakes (the gateway holds the request and wakes it).
	ModelAvailable
	// ModelSleepHeld: every active deployment serving it sleeps by an
	// operator's or a schedule's order, which a request does not wake.
	ModelSleepHeld
)

// Models looks a served model name up among the platform's deployments.
type Models interface {
	Lookup(name string) (ModelState, error)
}

// Reply is what sending one request came to.
type Reply struct {
	// Status is the HTTP status, 0 when no response came.
	Status int
	// RequestID is the gateway's id of the request.
	RequestID string
	// Body is the response body.
	Body []byte
	// RetryAfter is the delay the response asked for, zero for none.
	RetryAfter time.Duration
	// TooLarge: the response was larger than the gateway keeps for a reply.
	TooLarge bool
	// Err is why no response came: a transport failure, or the context.
	Err error
}

// Sender sends one request through the gateway as the owner's: the same
// routing, wake hold and accounting as a client's request.
type Sender interface {
	Send(ctx context.Context, owner Owner, path string, body []byte) Reply
}

// Clock is the time the executor reads and waits on.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives once d has passed.
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Error is a refusal carried to the client: an OpenAI-shaped error on /v1, a
// product error on the console's API.
type Error struct {
	Status  int
	Type    string
	Code    string
	Message string
	Param   string
	// Model is the model name a refusal about a model names.
	Model string
	// Line is the line of the input file the error is about, 0 for none.
	Line int
}

func (e *Error) Error() string { return e.Message }

func invalid(code, param, message string) *Error {
	return &Error{Status: 400, Type: "invalid_request_error", Code: code, Param: param, Message: message}
}

func notFound(code, message string) *Error {
	return &Error{Status: 404, Type: "invalid_request_error", Code: code, Message: message}
}
