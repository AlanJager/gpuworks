package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// A member that cannot commit a write here and now answers 503 so the edge
// tries the next member: a follower asked for a write only the scheduling
// leader may commit, or a member whose business writes are closed. The field
// saw model registration return 500 "scheduling generation changed" on both
// followers of a healthy three-member cluster.
func TestWriteErrAnswersNotHereNowWithServiceUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		err    error
		text   string
	}{
		{"follower write", http.StatusInternalServerError, store.ErrStaleSchedulingGeneration, "scheduling generation changed"},
		{"follower write given a conflict status", http.StatusConflict, fmt.Errorf("wrapped: %w", store.ErrStaleSchedulingGeneration), "wrapped: scheduling generation changed"},
		{"closed member", http.StatusInternalServerError, store.ErrBusinessWritesClosed, "business writes are closed"},
		{"closed member joined with a driver error", http.StatusBadGateway, errors.Join(store.ErrBusinessWritesClosed, errors.New("driver: bad connection")), "business writes are closed\ndriver: bad connection"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeErr(rec, tc.status, tc.err)
			var body struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if rec.Code != http.StatusServiceUnavailable || body.Error != tc.text || body.Code == "" {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// Ordinary errors keep the status their handler chose.
func TestWriteErrKeepsOrdinaryErrorStatus(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusConflict, http.StatusBadRequest} {
		rec := httptest.NewRecorder()
		writeErr(rec, status, errors.New("disk I/O error"))
		if rec.Code != status {
			t.Fatalf("status=%d want %d", rec.Code, status)
		}
	}
}
