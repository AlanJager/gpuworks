package api

import (
	"context"
	"database/sql/driver"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func (a *apiHandlers) logSettingWriteError(r *http.Request, err error) {
	if a.d.Log == nil {
		return
	}
	a.d.Log.DebugContext(r.Context(), "setting write failed",
		"request_id", middleware.GetReqID(r.Context()),
		"canceled", errors.Is(err, context.Canceled),
		"deadline_exceeded", errors.Is(err, context.DeadlineExceeded),
		"business_writes_closed", errors.Is(err, store.ErrBusinessWritesClosed),
		"bad_connection", errors.Is(err, driver.ErrBadConn),
		"caller_canceled", errors.Is(r.Context().Err(), context.Canceled),
		"caller_deadline_exceeded", errors.Is(r.Context().Err(), context.DeadlineExceeded))
}
