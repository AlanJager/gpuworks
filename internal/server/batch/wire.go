package batch

import "time"

// The OpenAI wire objects of /v1/files and /v1/batches. Times are Unix
// seconds; a field the object does not have yet is null, as OpenAI sends it.

type wireFile struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Bytes     int64  `json:"bytes"`
	CreatedAt int64  `json:"created_at"`
	Filename  string `json:"filename"`
	Purpose   string `json:"purpose"`
	Status    string `json:"status"`
}

func toWireFile(f File) wireFile {
	return wireFile{ID: f.ID, Object: "file", Bytes: f.Bytes, CreatedAt: f.CreatedAt.Unix(), Filename: f.Filename, Purpose: f.Purpose, Status: "processed"}
}

type wireFileList struct {
	Object  string     `json:"object"`
	Data    []wireFile `json:"data"`
	FirstID *string    `json:"first_id"`
	LastID  *string    `json:"last_id"`
	HasMore bool       `json:"has_more"`
}

type wireFileDeleted struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Deleted bool   `json:"deleted"`
}

type wireBatchError struct {
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Param   *string `json:"param"`
	Line    *int    `json:"line"`
}

type wireBatchErrors struct {
	Object string           `json:"object"`
	Data   []wireBatchError `json:"data"`
}

type wireCounts struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
}

type wireBatch struct {
	ID               string            `json:"id"`
	Object           string            `json:"object"`
	Endpoint         string            `json:"endpoint"`
	Errors           *wireBatchErrors  `json:"errors"`
	InputFileID      string            `json:"input_file_id"`
	CompletionWindow string            `json:"completion_window"`
	Status           string            `json:"status"`
	OutputFileID     *string           `json:"output_file_id"`
	ErrorFileID      *string           `json:"error_file_id"`
	CreatedAt        int64             `json:"created_at"`
	InProgressAt     *int64            `json:"in_progress_at"`
	ExpiresAt        int64             `json:"expires_at"`
	FinalizingAt     *int64            `json:"finalizing_at"`
	CompletedAt      *int64            `json:"completed_at"`
	FailedAt         *int64            `json:"failed_at"`
	ExpiredAt        *int64            `json:"expired_at"`
	CancellingAt     *int64            `json:"cancelling_at"`
	CancelledAt      *int64            `json:"cancelled_at"`
	RequestCounts    wireCounts        `json:"request_counts"`
	Metadata         map[string]string `json:"metadata"`
}

func toWireBatch(b Batch) wireBatch {
	w := wireBatch{
		ID: b.ID, Object: "batch", Endpoint: b.Endpoint, InputFileID: b.InputFileID,
		CompletionWindow: b.CompletionWindow, Status: b.Status,
		OutputFileID: nonEmpty(b.OutputFileID), ErrorFileID: nonEmpty(b.ErrorFileID),
		CreatedAt: b.CreatedAt.Unix(), ExpiresAt: b.ExpiresAt.Unix(),
		InProgressAt: unix(b.InProgressAt), FinalizingAt: unix(b.FinalizingAt), CompletedAt: unix(b.CompletedAt),
		FailedAt: unix(b.FailedAt), ExpiredAt: unix(b.ExpiredAt), CancellingAt: unix(b.CancellingAt), CancelledAt: unix(b.CancelledAt),
		RequestCounts: wireCounts{Total: b.Total, Completed: b.Completed, Failed: b.Failed},
	}
	if len(b.Metadata) > 0 {
		w.Metadata = b.Metadata
	}
	if b.ErrorCode != "" {
		w.Errors = &wireBatchErrors{Object: "list", Data: []wireBatchError{{Code: b.ErrorCode, Message: b.ErrorMessage}}}
	}
	return w
}

type wireBatchList struct {
	Object  string      `json:"object"`
	Data    []wireBatch `json:"data"`
	FirstID *string     `json:"first_id"`
	LastID  *string     `json:"last_id"`
	HasMore bool        `json:"has_more"`
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func unix(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	v := t.Unix()
	return &v
}
