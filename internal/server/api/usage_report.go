package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/usagereport"
)

// UsageReport is the user experience improvement program: the daily usage
// report (docs/reference/usage-report.md) and the counters that feed it.
type UsageReport interface {
	// Counters are the in-process counts the API adds to; nil counts nothing.
	Counters() *usagereport.Counters
	Status() dto.UsageReportStatus
	// SetEnabled saves usage_report in server.yaml and applies it at once.
	SetEnabled(on bool) error
	// Preview is the next report exactly as it would be sent.
	Preview(ctx context.Context) ([]byte, error)
}

// Notices are the messages for the console's notice bell: the publisher's
// (security advisories, releases, events) that apply to this installation,
// and the installation's own.
type Notices interface {
	List(userID uint) (dto.NoticeList, error)
	MarkRead(userID uint, ids []string) error
	// Refresh fetches the publisher's notices in the background unless they
	// were fetched recently; it returns at once. Update checks call it, so
	// notices are fetched exactly when the channel is.
	Refresh()
}

func (a *apiHandlers) usageCounters() *usagereport.Counters {
	if a.d.UsageReport == nil {
		return nil
	}
	return a.d.UsageReport.Counters()
}

func (a *apiHandlers) getUsageReport(w http.ResponseWriter, _ *http.Request) {
	if a.d.UsageReport == nil {
		writeJSON(w, http.StatusOK, dto.UsageReportStatus{})
		return
	}
	writeJSON(w, http.StatusOK, a.d.UsageReport.Status())
}

func (a *apiHandlers) putUsageReportEnabled(w http.ResponseWriter, r *http.Request) {
	var req dto.UsageReportEnabled
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
		writeProductError(w, http.StatusBadRequest, ErrorUsageReportInvalid, "enabled must be true or false", nil)
		return
	}
	if a.d.UsageReport == nil {
		writeProductError(w, http.StatusConflict, ErrorUsageReportReadOnly, "the server was started without server.yaml", nil)
		return
	}
	if err := a.d.UsageReport.SetEnabled(*req.Enabled); err != nil {
		writeServerSettingsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.d.UsageReport.Status())
}

// getUsageReportPreview answers the next report's bytes as they would be
// sent, so an administrator reads exactly what leaves the installation.
func (a *apiHandlers) getUsageReportPreview(w http.ResponseWriter, r *http.Request) {
	if a.d.UsageReport == nil {
		writeProductError(w, http.StatusNotFound, ErrorUsageReportUnavailable, "the usage report is not available on this server", nil)
		return
	}
	body, err := a.d.UsageReport.Preview(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (a *apiHandlers) getNotices(w http.ResponseWriter, r *http.Request) {
	if a.d.Notices == nil {
		writeJSON(w, http.StatusOK, dto.NoticeList{Notices: []dto.Notice{}})
		return
	}
	claims, _ := a.claimsFromRequest(r)
	list, err := a.d.Notices.List(claims.UserID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// maxNoticesRead bounds one read request; a console never has more notices.
const maxNoticesRead = 128

func (a *apiHandlers) postNoticesRead(w http.ResponseWriter, r *http.Request) {
	var req dto.NoticesRead
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.IDs) == 0 || len(req.IDs) > maxNoticesRead {
		writeProductError(w, http.StatusBadRequest, ErrorNoticeIDsInvalid, "ids must list between 1 and 128 notices", nil)
		return
	}
	for _, id := range req.IDs {
		if id == "" || len(id) > 64 {
			writeProductError(w, http.StatusBadRequest, ErrorNoticeIDsInvalid, "ids must list between 1 and 128 notices", nil)
			return
		}
	}
	if a.d.Notices == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	claims, _ := a.claimsFromRequest(r)
	if err := a.d.Notices.MarkRead(claims.UserID, req.IDs); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// usageErrorHeader carries a product error's code from writeErr to the
// counting middleware, which removes it before the response goes out.
const usageErrorHeader = "X-Gpuworks-Usage-Error"

// countUsage counts management API calls per route pattern, the product
// errors they answer per code, and the panics they raise per platform
// function, for the usage report. It only counts: the data plane (/v1) and
// everything outside /api/v1 pass untouched. It must sit inside the panic
// recoverer, which still answers the request.
func (a *apiHandlers) countUsage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counters := a.usageCounters()
		if counters == nil || !strings.HasPrefix(r.URL.Path, "/api/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		cw := &usageResponseWriter{ResponseWriter: w, counters: counters}
		defer func() {
			if rv := recover(); rv != nil {
				if rv != http.ErrAbortHandler { //nolint:errorlint // the sentinel is compared by identity
					counters.Inc(usagereport.KindPanic, usagereport.PanicSite())
				}
				panic(rv)
			}
			if rctx := chi.RouteContext(r.Context()); rctx != nil {
				// A request no route matched has no pattern worth counting.
				if pattern := strings.TrimPrefix(rctx.RoutePattern(), "/api/v1"); pattern != "" && !strings.HasSuffix(pattern, "*") {
					counters.Inc(usagereport.KindRoute, r.Method+" "+pattern)
				}
			}
		}()
		next.ServeHTTP(cw, r)
	})
}

// usageResponseWriter takes the product error code off a response as its
// header is written.
type usageResponseWriter struct {
	http.ResponseWriter
	counters *usagereport.Counters
	wrote    bool
}

func (w *usageResponseWriter) WriteHeader(status int) {
	if !w.wrote {
		w.wrote = true
		if code := w.Header().Get(usageErrorHeader); code != "" {
			w.Header().Del(usageErrorHeader)
			w.counters.Inc(usagereport.KindAPIError, code)
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *usageResponseWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the connection (flush, deadlines).
func (w *usageResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush keeps server-sent events streaming through the counter.
func (w *usageResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if !w.wrote {
			w.WriteHeader(http.StatusOK)
		}
		f.Flush()
	}
}

// Hijack keeps websocket upgrades (the web terminal) working through the
// counter.
func (w *usageResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response does not support hijacking")
	}
	w.wrote = true
	return h.Hijack()
}
